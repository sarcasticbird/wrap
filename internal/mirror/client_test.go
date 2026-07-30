package mirror

import (
	"context"
	"errors"
	"testing"
	"time"
)

type dispatchHandler struct {
	openErr  error
	onOpen   func(*Client)
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
func (dispatchHandler) Close(*Client) error                 { return nil }
func (dispatchHandler) Input(*Client, []byte) error         { return nil }
func (dispatchHandler) Resize(*Client, ResizeRequest) error { return nil }
func (dispatchHandler) Disconnected(*Client)                {}

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
		ID: "$7", Generation: "generation-a", Columns: 80, Rows: 24,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.dispatch(t.Context(), TagInput, []byte("x")); err == nil {
		t.Fatal("input without an open viewer was accepted")
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
	if err := client.dispatch(t.Context(), TagClose, nil); err == nil {
		t.Fatal("close without an open viewer was accepted")
	}
}

func TestClientDispatchTurnsOpenFailureIntoFixedEncryptedError(t *testing.T) {
	client := &Client{
		handler: dispatchHandler{openErr: errors.New("sensitive host detail")},
		queue:   newOutboundQueue(MaxClientQueueBytes),
	}
	open, err := EncodeControl(TagOpen, OpenRequest{
		ID: "$7", Generation: "generation-a", Columns: 80, Rows: 24,
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

func TestClientDispatchDoesNotOverwriteImmediateViewerClose(t *testing.T) {
	client := &Client{
		handler: dispatchHandler{onOpen: func(client *Client) {
			client.markViewerClosed()
		}},
		queue: newOutboundQueue(MaxClientQueueBytes),
	}
	open, err := EncodeControl(TagOpen, OpenRequest{
		ID: "$7", Generation: "generation-a", Columns: 80, Rows: 24,
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

func TestClientDispatchCloseQueuesStatusAcknowledgement(t *testing.T) {
	sessions := []Session{{
		ID: "$7", Generation: "generation-a", Name: "vb/api",
	}}
	client := &Client{
		handler: dispatchHandler{sessions: sessions},
		queue:   newOutboundQueue(MaxClientQueueBytes),
	}
	client.viewerOpen.Store(true)
	if err := client.dispatch(t.Context(), TagClose, nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	frame, ok := client.queue.pop(ctx)
	if !ok || frame.tag != TagStatus {
		t.Fatalf("close acknowledgement = %+v, %v", frame, ok)
	}
	var list SessionList
	if err := DecodeControl(frame.tag, frame.payload, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].ID != "$7" {
		t.Fatalf("close acknowledgement sessions = %+v", list.Sessions)
	}
}
