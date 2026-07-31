package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	"github.com/coder/websocket"
)

var ErrClientQueueFull = errors.New("mirror client outbound queue is full")

type outboundFrame struct {
	tag     byte
	payload []byte
	result  chan error
}

type outboundQueue struct {
	mu       sync.Mutex
	frames   []outboundFrame
	bytes    int
	maxBytes int
	closed   bool
	wake     chan struct{}
}

func newOutboundQueue(maxBytes int) *outboundQueue {
	return &outboundQueue{maxBytes: maxBytes, wake: make(chan struct{}, 1)}
}

func (q *outboundQueue) push(frame outboundFrame) error {
	size := len(frame.payload) + 1 + 16
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return errors.New("mirror client is closed")
	}
	if size > q.maxBytes || q.bytes+size > q.maxBytes {
		q.closeLocked(ErrClientQueueFull)
		return ErrClientQueueFull
	}
	frame.payload = append([]byte(nil), frame.payload...)
	q.frames = append(q.frames, frame)
	q.bytes += size
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return nil
}

func (q *outboundQueue) pop(ctx context.Context) (outboundFrame, bool) {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return outboundFrame{}, false
		}
		if len(q.frames) != 0 {
			frame := q.frames[0]
			q.frames[0] = outboundFrame{}
			q.frames = q.frames[1:]
			q.bytes -= len(frame.payload) + 1 + 16
			q.mu.Unlock()
			return frame, true
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return outboundFrame{}, false
		case <-q.wake:
		}
	}
}

func (q *outboundQueue) close(err error) {
	q.mu.Lock()
	q.closeLocked(err)
	q.mu.Unlock()
}

func (q *outboundQueue) closeLocked(err error) {
	if q.closed {
		return
	}
	q.closed = true
	for _, frame := range q.frames {
		if frame.result != nil {
			frame.result <- err
			close(frame.result)
		}
	}
	q.frames = nil
	q.bytes = 0
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

type Client struct {
	connection *websocket.Conn
	opener     *Opener
	sealer     *Sealer
	handler    ClientHandler

	queue      *outboundQueue
	closeOnce  sync.Once
	writerDone chan struct{}
	viewerOpen atomic.Bool
}

func authenticateClient(
	ctx context.Context,
	connection *websocket.Conn,
	secret Secret,
	random io.Reader,
	handler ClientHandler,
) (*Client, error) {
	serverNonce := make([]byte, 16)
	if _, err := io.ReadFull(random, serverNonce); err != nil {
		return nil, fmt.Errorf("generate server nonce: %w", err)
	}
	if err := connection.Write(ctx, websocket.MessageBinary, serverNonce); err != nil {
		return nil, fmt.Errorf("write server nonce: %w", err)
	}
	messageType, clientNonce, err := connection.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("read client nonce: %w", err)
	}
	if messageType != websocket.MessageBinary || len(clientNonce) != 16 {
		return nil, errors.New("invalid client nonce")
	}
	c2s, s2c, err := DeriveKeys(secret[:], serverNonce, clientNonce)
	if err != nil {
		return nil, err
	}
	opener, err := NewOpener(c2s)
	if err != nil {
		return nil, err
	}
	sealer, err := NewSealer(s2c)
	if err != nil {
		return nil, err
	}
	messageType, encryptedHello, err := connection.Read(ctx)
	if err != nil {
		return nil, fmt.Errorf("read encrypted client hello: %w", err)
	}
	if messageType != websocket.MessageBinary {
		return nil, errors.New("client hello must be binary")
	}
	tag, payload, err := opener.Open(encryptedHello)
	if err != nil || tag != TagClientHello {
		return nil, errors.New("invalid encrypted client hello")
	}
	var hello ClientHello
	if err := DecodeControl(tag, payload, &hello); err != nil {
		return nil, err
	}
	if err := ValidateClientFrame(tag, hello); err != nil {
		return nil, err
	}
	client := &Client{
		connection: connection,
		opener:     opener,
		sealer:     sealer,
		handler:    handler,
		queue:      newOutboundQueue(MaxClientQueueBytes),
		writerDone: make(chan struct{}),
	}
	return client, nil
}

func (c *Client) startWriter(ctx context.Context) {
	go c.writeLoop(ctx)
}

func (c *Client) SendControl(ctx context.Context, tag byte, value any) error {
	if err := ValidateServerFrame(tag, value); err != nil {
		return err
	}
	payload, err := EncodeControl(tag, value)
	if err != nil {
		return err
	}
	return c.send(ctx, tag, payload)
}

func (c *Client) SendOutput(ctx context.Context, output []byte) error {
	for _, chunk := range ChunkOutput(output) {
		if err := c.send(ctx, TagOutput, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) sendCloseAcknowledgement(ctx context.Context) error {
	if err := ValidateServerFrame(TagClose, nil); err != nil {
		return err
	}
	return c.send(ctx, TagClose, nil)
}

func (c *Client) send(ctx context.Context, tag byte, payload []byte) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	if err := c.queue.push(outboundFrame{tag: tag, payload: payload}); err != nil {
		c.closeNow(err)
		return err
	}
	return nil
}

func (c *Client) closeWithControl(ctx context.Context, tag byte, value any) error {
	if err := ValidateServerFrame(tag, value); err != nil {
		return err
	}
	payload, err := EncodeControl(tag, value)
	if err != nil {
		return err
	}
	result := make(chan error, 1)
	if err := c.queue.push(outboundFrame{tag: tag, payload: payload, result: result}); err != nil {
		c.closeNow(err)
		return err
	}
	select {
	case err := <-result:
		if err != nil {
			c.closeNow(err)
			return err
		}
		closed := make(chan error, 1)
		go func() {
			closed <- c.connection.Close(websocket.StatusNormalClosure, "mirror closed")
		}()
		select {
		case closeErr := <-closed:
			return normalizeWebSocketCloseError(closeErr)
		case <-ctx.Done():
			c.closeNow(ctx.Err())
			return ctx.Err()
		}
	case <-ctx.Done():
		c.closeNow(ctx.Err())
		return ctx.Err()
	}
}

func normalizeWebSocketCloseError(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

func (c *Client) writeLoop(ctx context.Context) {
	defer close(c.writerDone)
	for {
		frame, ok := c.queue.pop(ctx)
		if !ok {
			return
		}
		ciphertext, err := c.sealer.Seal(frame.tag, frame.payload)
		if err == nil {
			err = c.connection.Write(ctx, websocket.MessageBinary, ciphertext)
		}
		if frame.result != nil {
			frame.result <- err
			close(frame.result)
		}
		if err != nil {
			c.closeNow(err)
			return
		}
	}
}

func (c *Client) closeNow(err error) {
	c.closeOnce.Do(func() {
		c.queue.close(err)
		_ = c.connection.CloseNow()
	})
}

func (c *Client) run(ctx context.Context) {
	defer c.handler.Disconnected(c)
	defer c.closeNow(errors.New("mirror client disconnected"))
	for {
		messageType, ciphertext, err := c.connection.Read(ctx)
		if err != nil {
			return
		}
		if messageType != websocket.MessageBinary {
			return
		}
		tag, payload, err := c.opener.Open(ciphertext)
		if err != nil {
			return
		}
		if err := c.dispatch(ctx, tag, payload); err != nil {
			return
		}
	}
}

func (c *Client) dispatch(ctx context.Context, tag byte, payload []byte) error {
	switch tag {
	case TagOpen:
		var request OpenRequest
		if err := DecodeControl(tag, payload, &request); err != nil {
			return err
		}
		if err := ValidateClientFrame(tag, request); err != nil {
			return err
		}
		if !c.viewerOpen.CompareAndSwap(false, true) {
			return errors.New("a terminal is already open")
		}
		if err := c.handler.Open(ctx, c, request); err != nil {
			c.viewerOpen.Store(false)
			return c.SendControl(ctx, TagError, ProtocolError{
				Code:    "terminal_unavailable",
				Message: "The terminal is no longer available.",
				Retry:   true,
			})
		}
		return nil
	case TagClose:
		if err := ValidateClientFrame(tag, payload); err != nil {
			return err
		}
		if !c.viewerOpen.Load() {
			return c.sendCloseAcknowledgement(ctx)
		}
		if err := c.handler.Close(c); err != nil {
			if !c.viewerOpen.Load() {
				return c.sendCloseAcknowledgement(ctx)
			}
			return err
		}
		if !c.viewerOpen.CompareAndSwap(true, false) {
			return c.sendCloseAcknowledgement(ctx)
		}
		return c.sendCloseAcknowledgement(ctx)
	case TagInput:
		if err := ValidateClientFrame(tag, payload); err != nil {
			return err
		}
		if !c.viewerOpen.Load() {
			return nil
		}
		err := c.handler.Input(c, payload)
		if err != nil && !c.viewerOpen.Load() {
			return nil
		}
		return err
	case TagResize:
		var request ResizeRequest
		if err := DecodeControl(tag, payload, &request); err != nil {
			return err
		}
		if err := ValidateClientFrame(tag, request); err != nil {
			return err
		}
		if !c.viewerOpen.Load() {
			return nil
		}
		err := c.handler.Resize(c, request)
		if err != nil && !c.viewerOpen.Load() {
			return nil
		}
		return err
	default:
		return fmt.Errorf("tag 0x%02x is invalid from client", tag)
	}
}

func (c *Client) markViewerClosed() {
	c.viewerOpen.Store(false)
}
