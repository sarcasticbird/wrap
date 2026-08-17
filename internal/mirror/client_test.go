package mirror

import (
	"context"
	"errors"
	"testing"
	"time"
)

type dispatchHandler struct {
	closeCalls int
	inputCalls int
	closeErr   error
	inputErr   error
}

func (*dispatchHandler) Connected(context.Context, *Client) error { return nil }
func (h *dispatchHandler) Close(*Client) error {
	h.closeCalls++
	return h.closeErr
}
func (h *dispatchHandler) Input(*Client, []byte) error {
	h.inputCalls++
	return h.inputErr
}
func (*dispatchHandler) Disconnected(*Client) {}

func TestOutboundQueueIsFIFOAndByteBounded(t *testing.T) {
	queue := newOutboundQueue(40)
	if err := queue.push(outboundFrame{tag: TagOutput, payload: []byte("first")}); err != nil {
		t.Fatal(err)
	}
	if err := queue.push(outboundFrame{tag: TagOutput, payload: []byte("second")}); !errors.Is(err, ErrClientQueueFull) {
		t.Fatalf("second push = %v, want queue full", err)
	}
	if _, ok := queue.pop(t.Context()); ok {
		t.Fatal("closed full queue returned a frame")
	}
}

func TestOutboundQueueCopiesProducerPayload(t *testing.T) {
	queue := newOutboundQueue(1024)
	payload := []byte("safe")
	if err := queue.push(outboundFrame{tag: TagOutput, payload: payload}); err != nil {
		t.Fatal(err)
	}
	payload[0] = 'X'
	frame, ok := queue.pop(t.Context())
	if !ok || string(frame.payload) != "safe" {
		t.Fatalf("queued payload = %q, ok=%v", frame.payload, ok)
	}
}

func TestClientDispatchInputRequiresAutomaticallyOpenedViewer(t *testing.T) {
	handler := &dispatchHandler{}
	client := &Client{handler: handler, queue: newOutboundQueue(1024)}
	if err := client.dispatch(t.Context(), TagInput, []byte("before")); err != nil {
		t.Fatal(err)
	}
	if handler.inputCalls != 0 {
		t.Fatal("input reached handler before viewer opened")
	}
	client.viewerOpen.Store(true)
	if err := client.dispatch(t.Context(), TagInput, []byte("after")); err != nil {
		t.Fatal(err)
	}
	if handler.inputCalls != 1 {
		t.Fatalf("input calls = %d", handler.inputCalls)
	}
}

func TestClientDispatchCloseAcknowledgesAndClearsViewer(t *testing.T) {
	handler := &dispatchHandler{}
	client := &Client{handler: handler, queue: newOutboundQueue(1024)}
	client.viewerOpen.Store(true)
	if err := client.dispatch(t.Context(), TagClose, nil); err != nil {
		t.Fatal(err)
	}
	if client.viewerOpen.Load() || handler.closeCalls != 1 {
		t.Fatalf("close state = open:%v calls:%d", client.viewerOpen.Load(), handler.closeCalls)
	}
	frame, ok := client.queue.pop(t.Context())
	if !ok || frame.tag != TagClose || len(frame.payload) != 0 {
		t.Fatalf("close acknowledgement = %+v, ok=%v", frame, ok)
	}
}

func TestClientDispatchRejectsRemovedOpenFrame(t *testing.T) {
	client := &Client{handler: &dispatchHandler{}, queue: newOutboundQueue(1024)}
	if err := client.dispatch(t.Context(), 0x04, []byte(`{"id":"$1"}`)); err == nil {
		t.Fatal("removed open frame was accepted")
	}
}

func TestClientCloseFrameTimesOutStalledWriter(t *testing.T) {
	previous := closeFrameTimeout
	closeFrameTimeout = 20 * time.Millisecond
	t.Cleanup(func() { closeFrameTimeout = previous })
	client := &Client{queue: newOutboundQueue(1024)}
	started := time.Now()
	err := client.closeWithFrame(context.Background(), TagClose, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("closeWithFrame() = %v, want deadline exceeded", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("closeWithFrame() blocked for %s", time.Since(started))
	}
	if _, ok := client.queue.pop(t.Context()); ok {
		t.Fatal("timed-out close left the client queue open")
	}
}
