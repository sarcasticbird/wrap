package mirror

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

type dispatchHandler struct {
	openErr  error
	closeErr error
	inputErr error
	onOpen   func(*Client)
	onClose  func(*Client)
	onInput  func(*Client)
	sessions []Session
}

func (h dispatchHandler) InitialSessions() []Session { return h.sessions }
func (dispatchHandler) Connected(*Client)            {}
func (h dispatchHandler) Open(_ context.Context, client *Client, _ OpenRequest) error {
	if h.onOpen != nil {
		h.onOpen(client)
	}
	return h.openErr
}
func (h dispatchHandler) Close(client *Client) error {
	if h.onClose != nil {
		h.onClose(client)
	}
	return h.closeErr
}
func (h dispatchHandler) Input(client *Client, _ []byte) error {
	if h.onInput != nil {
		h.onInput(client)
	}
	return h.inputErr
}
func (dispatchHandler) Disconnected(*Client) {}

func TestOutboundQueueIsFIFOAndByteBounded(t *testing.T) {
	queue := newOutboundQueue(40)
	first := outboundFrame{tag: TagOutput, payload: []byte("one")}
	second := outboundFrame{tag: TagOutput, payload: []byte("two")}
	if err := queue.push(first); err != nil {
		t.Fatal(err)
	}
	if err := queue.push(second); err != nil {
		t.Fatal(err)
	}
	got, ok := queue.pop(t.Context())
	if !ok || got.tag != first.tag || string(got.payload) != "one" {
		t.Fatalf("first pop = %+v, %v", got, ok)
	}
	got, ok = queue.pop(t.Context())
	if !ok || got.tag != second.tag || string(got.payload) != "two" {
		t.Fatalf("second pop = %+v, %v", got, ok)
	}

	tooLarge := outboundFrame{tag: TagOutput, payload: make([]byte, 24)}
	if err := queue.push(tooLarge); !errors.Is(err, ErrClientQueueFull) {
		t.Fatalf("oversized push error = %v", err)
	}
	if _, ok := queue.pop(context.Background()); ok {
		t.Fatal("poisoned queue remained readable")
	}
}

func TestOutboundQueueCopiesProducerPayload(t *testing.T) {
	queue := newOutboundQueue(MaxClientQueueBytes)
	payload := []byte("terminal output")
	if err := queue.push(outboundFrame{tag: TagOutput, payload: payload}); err != nil {
		t.Fatal(err)
	}
	payload[0] = 'X'
	got, ok := queue.pop(t.Context())
	if !ok || string(got.payload) != "terminal output" {
		t.Fatalf("queued payload = %q, %v", got.payload, ok)
	}
}

func TestClientDispatchEnforcesOneOpenViewer(t *testing.T) {
	client := &Client{
		handler: dispatchHandler{},
		queue:   newOutboundQueue(MaxClientQueueBytes),
	}
	open, err := EncodeControl(TagOpen, OpenRequest{
		ID: "$7", Generation: "generation-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.dispatch(t.Context(), TagOpen, open); err != nil {
		t.Fatal(err)
	}
	if err := client.dispatch(t.Context(), TagOpen, open); err == nil {
		t.Fatal("a second open without close was accepted")
	}
	if err := client.dispatch(t.Context(), TagClose, nil); err != nil {
		t.Fatal(err)
	}
}

func TestClientDispatchDropsFramesForViewerThatJustEnded(t *testing.T) {
	for _, test := range []struct {
		name    string
		tag     byte
		payload []byte
	}{
		{name: "close", tag: TagClose},
		{name: "input", tag: TagInput, payload: []byte("x")},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{
				handler: dispatchHandler{},
				queue:   newOutboundQueue(MaxClientQueueBytes),
			}
			if err := client.dispatch(t.Context(), test.tag, test.payload); err != nil {
				t.Fatalf("stale viewer frame closed client: %v", err)
			}
		})
	}
}

func TestClientDispatchAcknowledgesCloseAfterViewerEnded(t *testing.T) {
	client := &Client{
		handler: dispatchHandler{},
		queue:   newOutboundQueue(MaxClientQueueBytes),
	}
	if err := client.dispatch(t.Context(), TagClose, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	frame, ok := client.queue.pop(ctx)
	if !ok || frame.tag != TagClose || len(frame.payload) != 0 {
		t.Fatalf("stale close acknowledgement = %+v, %v", frame, ok)
	}
}

func TestClientDispatchValidatesFramesForViewerThatJustEnded(t *testing.T) {
	for _, test := range []struct {
		name    string
		tag     byte
		payload []byte
	}{
		{name: "non-empty close", tag: TagClose, payload: []byte("x")},
		{name: "oversized input", tag: TagInput, payload: make([]byte, MaxWireMessage)},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{
				handler: dispatchHandler{},
				queue:   newOutboundQueue(MaxClientQueueBytes),
			}
			if err := client.dispatch(t.Context(), test.tag, test.payload); err == nil {
				t.Fatal("invalid stale viewer frame was accepted")
			}
		})
	}
}

func TestClientDispatchDropsHandlerErrorAfterConcurrentViewerEnd(t *testing.T) {
	handlerErr := errors.New("no terminal is open")
	for _, test := range []struct {
		name    string
		tag     byte
		payload []byte
		handler dispatchHandler
	}{
		{
			name: "close", tag: TagClose,
			handler: dispatchHandler{
				closeErr: handlerErr,
				onClose:  func(client *Client) { client.markViewerClosed() },
			},
		},
		{
			name: "input", tag: TagInput, payload: []byte("x"),
			handler: dispatchHandler{
				inputErr: handlerErr,
				onInput:  func(client *Client) { client.markViewerClosed() },
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{
				handler: test.handler,
				queue:   newOutboundQueue(MaxClientQueueBytes),
			}
			client.viewerOpen.Store(true)
			if err := client.dispatch(t.Context(), test.tag, test.payload); err != nil {
				t.Fatalf("concurrent viewer end closed client: %v", err)
			}
		})
	}
}

func TestClientDispatchPreservesHandlerErrorWhileViewerIsOpen(t *testing.T) {
	handlerErr := errors.New("viewer write failed")
	client := &Client{
		handler: dispatchHandler{inputErr: handlerErr},
		queue:   newOutboundQueue(MaxClientQueueBytes),
	}
	client.viewerOpen.Store(true)
	if err := client.dispatch(t.Context(), TagInput, []byte("x")); !errors.Is(err, handlerErr) {
		t.Fatalf("active viewer input error = %v, want %v", err, handlerErr)
	}
}

func TestNormalizeWebSocketCloseErrorTreatsClosedConnectionAsSuccess(t *testing.T) {
	wrappedClosed := fmt.Errorf("failed to close WebSocket: %w", net.ErrClosed)
	if err := normalizeWebSocketCloseError(wrappedClosed); err != nil {
		t.Fatalf("closed connection cleanup error = %v", err)
	}
	other := errors.New("close handshake timed out")
	if err := normalizeWebSocketCloseError(other); !errors.Is(err, other) {
		t.Fatalf("non-closure error = %v, want %v", err, other)
	}
}

func TestClientDispatchTurnsOpenFailureIntoFixedEncryptedError(t *testing.T) {
	client := &Client{
		handler: dispatchHandler{openErr: errors.New("sensitive host detail")},
		queue:   newOutboundQueue(MaxClientQueueBytes),
	}
	open, err := EncodeControl(TagOpen, OpenRequest{
		ID: "$7", Generation: "generation-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.dispatch(t.Context(), TagOpen, open); err != nil {
		t.Fatal(err)
	}
	frame, ok := client.queue.pop(t.Context())
	if !ok || frame.tag != TagError {
		t.Fatalf("safe error frame = %+v, %v", frame, ok)
	}
	var problem ProtocolError
	if err := DecodeControl(frame.tag, frame.payload, &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "terminal_unavailable" ||
		problem.Message != "The terminal is no longer available." ||
		!problem.Retry {
		t.Fatalf("safe error = %+v", problem)
	}
}

func TestClientDispatchDropsOpenErrorAfterConcurrentViewerEnd(t *testing.T) {
	client := &Client{
		handler: dispatchHandler{
			openErr: errors.New("terminal disappeared during open"),
			onOpen:  func(client *Client) { client.markViewerClosed() },
		},
		queue: newOutboundQueue(MaxClientQueueBytes),
	}
	open, err := EncodeControl(TagOpen, OpenRequest{
		ID: "$7", Generation: "generation-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.dispatch(t.Context(), TagOpen, open); err != nil {
		t.Fatalf("concurrent viewer end closed client: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if frame, ok := client.queue.pop(ctx); ok {
		t.Fatalf("concurrent viewer end queued stale frame: %+v", frame)
	}
}

func TestClientDispatchDoesNotOverwriteImmediateViewerClose(t *testing.T) {
	client := &Client{
		handler: dispatchHandler{onOpen: func(client *Client) {
			client.markViewerClosed()
		}},
		queue: newOutboundQueue(MaxClientQueueBytes),
	}
	open, err := EncodeControl(TagOpen, OpenRequest{
		ID: "$7", Generation: "generation-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.dispatch(t.Context(), TagOpen, open); err != nil {
		t.Fatal(err)
	}
	if client.viewerOpen.Load() {
		t.Fatal("immediate viewer exit was overwritten by open completion")
	}
}

func TestClientDispatchCloseAcknowledgementFollowsQueuedStatus(t *testing.T) {
	sessions := []Session{{
		ID: "$7", Generation: "generation-a", Name: "vb/api",
	}}
	client := &Client{
		handler: dispatchHandler{sessions: sessions},
		queue:   newOutboundQueue(MaxClientQueueBytes),
	}
	client.viewerOpen.Store(true)
	if err := client.SendControl(t.Context(), TagStatus, SessionList{Sessions: sessions}); err != nil {
		t.Fatal(err)
	}
	if err := client.dispatch(t.Context(), TagClose, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	frame, ok := client.queue.pop(ctx)
	if !ok || frame.tag != TagStatus {
		t.Fatalf("status before close acknowledgement = %+v, %v", frame, ok)
	}
	frame, ok = client.queue.pop(ctx)
	if !ok || frame.tag != TagClose {
		t.Fatalf("close acknowledgement = %+v, %v", frame, ok)
	}
	if len(frame.payload) != 0 {
		t.Fatalf("close acknowledgement payload = %x", frame.payload)
	}
}
