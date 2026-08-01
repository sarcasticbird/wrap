package mirror

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeTunnelResource struct {
	url    string
	done   chan error
	closed atomic.Int32
}

type cancelOrderTunnelResource struct {
	ctx         context.Context
	done        chan error
	diagnostics *recordingDiagnosticSink
}

func (t *cancelOrderTunnelResource) URL() string        { return "https://quiet-river.trycloudflare.com" }
func (t *cancelOrderTunnelResource) Done() <-chan error { return t.done }
func (t *cancelOrderTunnelResource) Close() error {
	select {
	case <-t.ctx.Done():
		_ = t.diagnostics.Write(DiagnosticRecord{
			Level: "error", Component: "tunnel", Event: "process_exit", Code: "unexpected_exit",
		})
	default:
	}
	return nil
}

func (t *fakeTunnelResource) URL() string        { return t.url }
func (t *fakeTunnelResource) Done() <-chan error { return t.done }
func (t *fakeTunnelResource) Close() error {
	t.closed.Add(1)
	return nil
}

type fakeServerResource struct {
	localURL string
	closed   atomic.Int32

	mu         sync.Mutex
	publicHost string
	secrets    []Secret
}

func (s *fakeServerResource) LocalURL() string { return s.localURL }
func (s *fakeServerResource) SetPublicHost(host string) {
	s.mu.Lock()
	s.publicHost = host
	s.mu.Unlock()
}
func (s *fakeServerResource) SetSecret(secret Secret) {
	s.mu.Lock()
	s.secrets = append(s.secrets, secret)
	s.mu.Unlock()
}
func (s *fakeServerResource) Close(context.Context) error {
	s.closed.Add(1)
	return nil
}

type viewerFactoryFunc func(
	context.Context,
	Identity,
	func([]byte) error,
) (PreparedViewer, error)

func (f viewerFactoryFunc) Prepare(
	ctx context.Context,
	identity Identity,
	output func([]byte) error,
) (PreparedViewer, error) {
	return f(ctx, identity, output)
}

type fakePreparedViewer struct {
	geometry ViewerGeometry
	viewer   Viewer
	startErr error
	closed   atomic.Int32
	onStart  func()
}

type acknowledgerFunc func(id, generation string) error

func (f acknowledgerFunc) AcknowledgeSession(id, generation string) error {
	return f(id, generation)
}

func (p *fakePreparedViewer) Geometry() ViewerGeometry { return p.geometry }
func (p *fakePreparedViewer) Start() (Viewer, error) {
	if p.onStart != nil {
		p.onStart()
	}
	return p.viewer, p.startErr
}
func (p *fakePreparedViewer) Close() error {
	p.closed.Add(1)
	return nil
}

func prepareFakeViewer(viewer Viewer) PreparedViewer {
	return &fakePreparedViewer{
		geometry: ViewerGeometry{Columns: 80, Rows: 24},
		viewer:   viewer,
	}
}

type fakeViewer struct {
	done chan error
}

type diagnosticViewer struct {
	done chan error
	once sync.Once
}

func newDiagnosticViewer() *diagnosticViewer {
	return &diagnosticViewer{done: make(chan error)}
}

func (v *diagnosticViewer) Write([]byte) error { return nil }
func (v *diagnosticViewer) Close() error {
	v.once.Do(func() { close(v.done) })
	return nil
}
func (v *diagnosticViewer) Done() <-chan error { return v.done }

func (v *fakeViewer) Write([]byte) error { return nil }
func (v *fakeViewer) Close() error       { return nil }
func (v *fakeViewer) Done() <-chan error { return v.done }

func TestManagerSuccessfulStartOwnsRuntimeContext(t *testing.T) {
	server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	serverContext := make(chan context.Context, 1)
	tunnelContext := make(chan context.Context, 1)
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		StartServer: func(ctx context.Context, _ ServerOptions) (ServerResource, error) {
			serverContext <- ctx
			return server, nil
		},
		StartTunnel: func(ctx context.Context, _ string) (TunnelResource, error) {
			tunnelContext <- ctx
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	operationContext, cancelOperation := context.WithCancel(t.Context())
	if err := manager.Mirror(operationContext, HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}); err != nil {
		t.Fatal(err)
	}
	runtimeContexts := []context.Context{<-serverContext, <-tunnelContext}

	cancelOperation()
	for _, runtimeContext := range runtimeContexts {
		select {
		case <-runtimeContext.Done():
			t.Fatalf("successful mirror retained the operation context: %v", runtimeContext.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
	if got := manager.Snapshot().State; got != StateReady {
		t.Fatalf("state after operation context cancellation = %v", got)
	}

	if err := manager.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, runtimeContext := range runtimeContexts {
		select {
		case <-runtimeContext.Done():
		case <-time.After(time.Second):
			t.Fatal("shutdown did not cancel the mirror runtime context")
		}
	}
}

func TestManagerDefaultServerRecordsStartedOnce(t *testing.T) {
	sink := &recordingDiagnosticSink{}
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	manager, err := NewManager(ManagerOptions{
		Workspace:   "vb",
		Diagnostics: sink,
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Mirror(t.Context(), HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	started := 0
	for _, record := range sink.snapshot() {
		if record.Component == "server" && record.Event == "started" {
			started++
		}
	}
	if started != 1 {
		t.Fatalf("server started diagnostics = %d, want 1: %+v", started, sink.snapshot())
	}
}

func TestManagerCleanupClosesTunnelBeforeCancelingRuntime(t *testing.T) {
	for _, operation := range []string{"shutdown", "revoke"} {
		t.Run(operation, func(t *testing.T) {
			sink := &recordingDiagnosticSink{}
			server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
			var tunnel *cancelOrderTunnelResource
			manager, err := NewManager(ManagerOptions{
				Workspace:   "vb",
				Diagnostics: sink,
				StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
					return server, nil
				},
				StartTunnel: func(ctx context.Context, _ string) (TunnelResource, error) {
					tunnel = &cancelOrderTunnelResource{
						ctx: ctx, done: make(chan error), diagnostics: sink,
					}
					return tunnel, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			session := HostSession{
				ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
			}
			if err := manager.Mirror(t.Context(), session); err != nil {
				t.Fatal(err)
			}
			if operation == "shutdown" {
				err = manager.Shutdown(t.Context())
			} else {
				err = manager.Revoke(t.Context(), Identity{ID: session.ID, Generation: session.Generation})
			}
			if err != nil {
				t.Fatal(err)
			}
			if containsDiagnostic(sink.snapshot(), "tunnel", "process_exit", "unexpected_exit") {
				t.Fatalf("intentional %s recorded unexpected tunnel exit: %+v", operation, sink.snapshot())
			}
		})
	}
}

func TestManagerStartsOnceAndLastRevokeTearsDown(t *testing.T) {
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	var starts atomic.Int32
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			starts.Add(1)
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	if got := manager.Snapshot().State; got != StateStopped {
		t.Fatalf("initial state = %v", got)
	}

	first := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef",
		Name: "vb/api", Kind: "entry", Activity: 10, SeenActivity: 10,
	}
	if err := manager.Mirror(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	if snapshot.State != StateReady || len(snapshot.Sessions) != 1 {
		t.Fatalf("ready snapshot = %+v", snapshot)
	}
	if !strings.HasPrefix(snapshot.PairingURL, "https://quiet-river.trycloudflare.com/#k=") ||
		len(strings.TrimPrefix(snapshot.PairingURL, "https://quiet-river.trycloudflare.com/#k=")) != 43 {
		t.Fatalf("pairing URL = %q", snapshot.PairingURL)
	}

	second := HostSession{
		ID: "$8", Generation: first.Generation, Name: "vb·term·1", Kind: "scratch",
	}
	if err := manager.Mirror(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || len(manager.Snapshot().Sessions) != 2 {
		t.Fatalf("starts/sessions = %d/%d", starts.Load(), len(manager.Snapshot().Sessions))
	}
	if err := manager.Revoke(t.Context(), Identity{ID: first.ID, Generation: first.Generation}); err != nil {
		t.Fatal(err)
	}
	if manager.Snapshot().State != StateReady {
		t.Fatal("revoking one of two sessions stopped the workspace mirror")
	}
	if err := manager.Revoke(t.Context(), Identity{ID: second.ID, Generation: second.Generation}); err != nil {
		t.Fatal(err)
	}
	if manager.Snapshot().State != StateStopped || tunnel.closed.Load() != 1 {
		t.Fatalf("last revoke state/close = %v/%d", manager.Snapshot().State, tunnel.closed.Load())
	}
}

func TestManagerRotateRetainsResourcesAndChangesPairingCredential(t *testing.T) {
	server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
			return server, nil
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	session := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}
	if err := manager.Mirror(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	before := manager.Snapshot()

	if err := manager.Rotate(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := manager.Snapshot()
	if after.State != StateReady || after.PublicURL != before.PublicURL {
		t.Fatalf("rotated state = %+v", after)
	}
	if after.PairingURL == before.PairingURL || after.QR == before.QR {
		t.Fatal("rotation did not replace the pairing credential")
	}
	if server.closed.Load() != 0 || tunnel.closed.Load() != 0 {
		t.Fatalf("rotation closed server/tunnel = %d/%d", server.closed.Load(), tunnel.closed.Load())
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.secrets) != 1 {
		t.Fatalf("server secret updates = %d, want 1", len(server.secrets))
	}
}

func TestManagerReconcileRevokesDisappearedAndChangedGeneration(t *testing.T) {
	var servers []*fakeServerResource
	var tunnels []*fakeTunnelResource
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
			server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
			servers = append(servers, server)
			return server, nil
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			tunnel := &fakeTunnelResource{
				url:  "https://quiet-river.trycloudflare.com",
				done: make(chan error),
			}
			tunnels = append(tunnels, tunnel)
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	generation := "0123456789abcdef0123456789abcdef"
	session := HostSession{ID: "$7", Generation: generation, Name: "vb/api"}
	if err := manager.Reconcile(t.Context(), []HostSession{session}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Mirror(t.Context(), session); err != nil {
		t.Fatal(err)
	}

	updated := session
	updated.Name = "vb/web"
	updated.Activity = 2
	updated.SeenActivity = 1
	if err := manager.Reconcile(t.Context(), []HostSession{updated}); err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	if len(snapshot.Sessions) != 1 ||
		snapshot.Sessions[0].Name != "vb/web" ||
		!snapshot.Sessions[0].Activity {
		t.Fatalf("updated sessions = %+v", snapshot.Sessions)
	}

	nextGeneration := "fedcba9876543210fedcba9876543210"
	if err := manager.Reconcile(t.Context(), []HostSession{{
		ID: "$7", Generation: nextGeneration, Name: "vb/new",
	}}); err != nil {
		t.Fatal(err)
	}
	snapshot = manager.Snapshot()
	if snapshot.State != StateStopped || len(snapshot.Sessions) != 0 {
		t.Fatalf("generation-change snapshot = %+v", snapshot)
	}
	if servers[0].closed.Load() != 1 || tunnels[0].closed.Load() != 1 {
		t.Fatalf("generation-change closes = %d/%d", servers[0].closed.Load(), tunnels[0].closed.Load())
	}
}

func TestManagerFailedTunnelStartClosesPartialServer(t *testing.T) {
	server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
			return server, nil
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return nil, errors.New("tunnel refused")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.Mirror(t.Context(), HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	})
	if err == nil || !strings.Contains(err.Error(), "tunnel refused") {
		t.Fatalf("mirror error = %v", err)
	}
	if server.closed.Load() != 1 {
		t.Fatalf("partial server closes = %d", server.closed.Load())
	}
	snapshot := manager.Snapshot()
	if snapshot.State != StateFailed || len(snapshot.Sessions) != 0 {
		t.Fatalf("failed snapshot = %+v", snapshot)
	}
}

func TestManagerUnexpectedTunnelExitFailsClosed(t *testing.T) {
	server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error, 1),
	}
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
			return server, nil
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Mirror(t.Context(), HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}); err != nil {
		t.Fatal(err)
	}

	tunnel.done <- errors.New("process exited")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		snapshot := manager.Snapshot()
		if snapshot.State == StateFailed {
			if len(snapshot.Sessions) != 0 ||
				!strings.Contains(snapshot.Err, "Quick Tunnel exited") {
				t.Fatalf("tunnel-exit snapshot = %+v", snapshot)
			}
			if server.closed.Load() != 1 || tunnel.closed.Load() != 1 {
				t.Fatalf(
					"tunnel-exit closes = server:%d tunnel:%d",
					server.closed.Load(),
					tunnel.closed.Load(),
				)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("manager did not fail after tunnel exit: %+v", manager.Snapshot())
}

func TestManagerDiagnosticFailureWarnsOnceWithoutStoppingMirror(t *testing.T) {
	server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	var writes atomic.Int32
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		Diagnostics: DiagnosticSinkFunc(func(DiagnosticRecord) error {
			writes.Add(1)
			return errors.New("SENTINEL diagnostics filesystem failure")
		}),
		StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
			return server, nil
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	if err := manager.Mirror(t.Context(), HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Snapshot()
	if snapshot.State != StateReady || snapshot.DiagnosticsWarning != "diagnostics unavailable" {
		t.Fatalf("mirror snapshot after diagnostic failure = %+v", snapshot)
	}
	if strings.Contains(snapshot.DiagnosticsWarning, "SENTINEL") {
		t.Fatal("diagnostic warning exposed the raw sink error")
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("diagnostic writes after failure = %d, want 1", got)
	}
	manager.recordDiagnostic(DiagnosticRecord{Level: "info", Component: "server", Event: "stopped"})
	if got := writes.Load(); got != 1 {
		t.Fatalf("disabled diagnostic sink retried %d writes", got)
	}
}

func TestManagerWiresDefaultViewerDiagnostics(t *testing.T) {
	sink := &recordingDiagnosticSink{}
	manager, err := NewManager(ManagerOptions{
		Workspace:   "vb",
		Diagnostics: sink,
	})
	if err != nil {
		t.Fatal(err)
	}
	factory, ok := manager.viewers.(*PTYViewerFactory)
	if !ok {
		t.Fatalf("default viewers = %T, want *PTYViewerFactory", manager.viewers)
	}
	if factory.Record == nil {
		t.Fatal("default viewer factory has no diagnostic recorder")
	}
	factory.Record(DiagnosticRecord{
		Level:     "info",
		Component: "viewer",
		Event:     "geometry_preparing",
	})
	if !containsDiagnostic(sink.snapshot(), "viewer", "geometry_preparing", "") {
		t.Fatal("default viewer diagnostic did not reach manager sink")
	}
}

func TestManagerRecordsSafeMirrorLifecycle(t *testing.T) {
	sink := &recordingDiagnosticSink{}
	server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	viewer := newDiagnosticViewer()
	manager, err := NewManager(ManagerOptions{
		Workspace:   "vb",
		Diagnostics: sink,
		Viewers: viewerFactoryFunc(func(
			context.Context, Identity, func([]byte) error,
		) (PreparedViewer, error) {
			return prepareFakeViewer(viewer), nil
		}),
		StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
			return server, nil
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef",
		Name: "SENTINEL_PRIVATE_SESSION_NAME",
	}
	if err := manager.Mirror(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	if err := manager.Rotate(t.Context()); err != nil {
		t.Fatal(err)
	}
	client := &Client{queue: newOutboundQueue(MaxClientQueueBytes)}
	manager.Connected(client)
	if _, ok := client.queue.pop(t.Context()); !ok {
		t.Fatal("client did not receive status")
	}
	if err := manager.Open(t.Context(), client, OpenRequest{
		ID: session.ID, Generation: session.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Close(client); err != nil {
		t.Fatal(err)
	}
	manager.Disconnected(client)
	if err := manager.Revoke(t.Context(), Identity{ID: session.ID, Generation: session.Generation}); err != nil {
		t.Fatal(err)
	}

	records := sink.snapshot()
	for _, want := range []struct{ component, event string }{
		{"server", "starting"},
		{"tunnel", "starting"},
		{"credential", "rotated"},
		{"viewer", "preparing"},
		{"viewer", "opened"},
		{"viewer", "closed"},
		{"credential", "revoked"},
	} {
		if !containsDiagnostic(records, want.component, want.event, "") {
			t.Fatalf("missing manager diagnostic %s/%s in %+v", want.component, want.event, records)
		}
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("SENTINEL")) {
		t.Fatalf("manager diagnostics leaked session identity: %s", encoded)
	}
}

func TestManagerQueuesOpenedBeforeViewerOutput(t *testing.T) {
	viewer := &fakeViewer{done: make(chan error, 1)}
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		Viewers: viewerFactoryFunc(func(
			_ context.Context,
			_ Identity,
			output func([]byte) error,
		) (PreparedViewer, error) {
			return &fakePreparedViewer{
				geometry: ViewerGeometry{Columns: 132, Rows: 41},
				viewer:   viewer,
				onStart:  func() { _ = output([]byte("first output")) },
			}, nil
		}),
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}
	if err := manager.Mirror(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	client := &Client{queue: newOutboundQueue(MaxClientQueueBytes)}
	t.Cleanup(func() {
		manager.Disconnected(client)
		_ = manager.Shutdown(context.Background())
	})
	manager.Connected(client)
	if _, ok := client.queue.pop(t.Context()); !ok {
		t.Fatal("client did not receive initial status")
	}
	if err := manager.Open(t.Context(), client, OpenRequest{
		ID: session.ID, Generation: session.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	openedFrame, ok := client.queue.pop(t.Context())
	if !ok || openedFrame.tag != TagOpened {
		t.Fatalf("first viewer frame = %+v, %v; want opened", openedFrame, ok)
	}
	var opened Opened
	if err := DecodeControl(openedFrame.tag, openedFrame.payload, &opened); err != nil {
		t.Fatal(err)
	}
	if opened != (Opened{
		ID: session.ID, Generation: session.Generation, Columns: 132, Rows: 41,
	}) {
		t.Fatalf("opened geometry = %+v", opened)
	}
	outputFrame, ok := client.queue.pop(t.Context())
	if !ok || outputFrame.tag != TagOutput || string(outputFrame.payload) != "first output" {
		t.Fatalf("second viewer frame = %+v, %v; want first output", outputFrame, ok)
	}
}

func TestManagerDoesNotAcknowledgeAttentionBeforeViewerStarts(t *testing.T) {
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	prepared := &fakePreparedViewer{
		geometry: ViewerGeometry{Columns: 80, Rows: 24},
		startErr: errors.New("viewer start failed"),
	}
	var acknowledgements atomic.Int32
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		Acknowledger: acknowledgerFunc(func(string, string) error {
			acknowledgements.Add(1)
			return nil
		}),
		Viewers: viewerFactoryFunc(func(
			context.Context, Identity, func([]byte) error,
		) (PreparedViewer, error) {
			return prepared, nil
		}),
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}
	if err := manager.Mirror(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	client := &Client{queue: newOutboundQueue(MaxClientQueueBytes)}
	manager.Connected(client)
	if _, ok := client.queue.pop(t.Context()); !ok {
		t.Fatal("client did not receive initial status")
	}
	if err := manager.Open(t.Context(), client, OpenRequest{
		ID: session.ID, Generation: session.Generation,
	}); err == nil {
		t.Fatal("viewer unexpectedly opened")
	}
	if got := acknowledgements.Load(); got != 0 {
		t.Fatalf("attention acknowledgements = %d, want 0", got)
	}
	if got := prepared.closed.Load(); got != 1 {
		t.Fatalf("prepared viewer closes = %d, want 1", got)
	}
	manager.Disconnected(client)
	if err := manager.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRevokeDoesNotWaitForPendingViewerStart(t *testing.T) {
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	viewer := newDiagnosticViewer()
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		Viewers: viewerFactoryFunc(func(
			_ context.Context, _ Identity, output func([]byte) error,
		) (PreparedViewer, error) {
			return &fakePreparedViewer{
				geometry: ViewerGeometry{Columns: 80, Rows: 24},
				viewer:   viewer,
				onStart: func() {
					close(startEntered)
					<-releaseStart
					_ = output([]byte("late output"))
				},
			}, nil
		}),
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}
	second := HostSession{
		ID: "$8", Generation: first.Generation, Name: "vb/web",
	}
	if err := manager.Mirror(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := manager.Mirror(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	client := &Client{queue: newOutboundQueue(MaxClientQueueBytes)}
	manager.Connected(client)
	if _, ok := client.queue.pop(t.Context()); !ok {
		t.Fatal("client did not receive initial status")
	}
	client.viewerOpen.Store(true)
	openResult := make(chan error, 1)
	go func() {
		openResult <- manager.Open(t.Context(), client, OpenRequest{
			ID: first.ID, Generation: first.Generation,
		})
	}()
	<-startEntered
	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- manager.Revoke(t.Context(), Identity{
			ID: first.ID, Generation: first.Generation,
		})
	}()
	revokeBlocked := false
	select {
	case err := <-revokeResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		revokeBlocked = true
	}
	close(releaseStart)
	if revokeBlocked {
		if err := <-revokeResult; err != nil {
			t.Fatal(err)
		}
		t.Fatal("revoke blocked on pending viewer startup")
	}
	if err := <-openResult; err == nil {
		t.Fatal("revoked pending viewer unexpectedly opened")
	}
	select {
	case <-viewer.done:
	case <-time.After(time.Second):
		t.Fatal("revoked pending viewer was not closed after startup returned")
	}
	if client.viewerOpen.Load() {
		t.Fatal("revoke left pending client marked open")
	}
	for {
		frame, ok := client.queue.pop(context.Background())
		if !ok {
			break
		}
		if frame.tag == TagOutput {
			t.Fatal("canceled pending viewer queued terminal output")
		}
		client.queue.mu.Lock()
		remaining := len(client.queue.frames)
		client.queue.mu.Unlock()
		if remaining == 0 {
			break
		}
	}
	manager.Disconnected(client)
	if err := manager.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerSnapshotDoesNotWaitForViewerAcknowledgement(t *testing.T) {
	ackEntered := make(chan struct{})
	releaseAck := make(chan struct{})
	viewer := newDiagnosticViewer()
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		Acknowledger: acknowledgerFunc(func(string, string) error {
			close(ackEntered)
			<-releaseAck
			return nil
		}),
		Viewers: viewerFactoryFunc(func(
			context.Context, Identity, func([]byte) error,
		) (PreparedViewer, error) {
			return prepareFakeViewer(viewer), nil
		}),
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}
	if err := manager.Mirror(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	client := &Client{queue: newOutboundQueue(MaxClientQueueBytes)}
	manager.Connected(client)
	if _, ok := client.queue.pop(t.Context()); !ok {
		t.Fatal("client did not receive initial status")
	}
	openResult := make(chan error, 1)
	go func() {
		openResult <- manager.Open(t.Context(), client, OpenRequest{
			ID: session.ID, Generation: session.Generation,
		})
	}()
	<-ackEntered
	snapshotResult := make(chan Snapshot, 1)
	go func() { snapshotResult <- manager.Snapshot() }()
	snapshotBlocked := false
	select {
	case snapshot := <-snapshotResult:
		if snapshot.State != StateReady {
			t.Fatalf("snapshot during acknowledgement = %+v", snapshot)
		}
	case <-time.After(100 * time.Millisecond):
		snapshotBlocked = true
	}
	close(releaseAck)
	if err := <-openResult; err != nil {
		t.Fatal(err)
	}
	if snapshotBlocked {
		<-snapshotResult
		t.Fatal("snapshot blocked on viewer acknowledgement")
	}
	manager.Disconnected(client)
	if err := manager.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerQueuesOpenedBeforeConcurrentRevocation(t *testing.T) {
	sendEntered := make(chan struct{})
	releaseSend := make(chan struct{})
	viewer := newDiagnosticViewer()
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		Viewers: viewerFactoryFunc(func(
			context.Context, Identity, func([]byte) error,
		) (PreparedViewer, error) {
			return prepareFakeViewer(viewer), nil
		}),
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	first := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}
	second := HostSession{ID: "$8", Generation: first.Generation, Name: "vb/web"}
	if err := manager.Mirror(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	if err := manager.Mirror(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	client := &Client{queue: newOutboundQueue(MaxClientQueueBytes)}
	manager.Connected(client)
	if _, ok := client.queue.pop(t.Context()); !ok {
		t.Fatal("client did not receive initial status")
	}
	client.beforeSend = func(tag byte) {
		if tag == TagOpened {
			close(sendEntered)
			<-releaseSend
		}
	}
	openResult := make(chan error, 1)
	go func() {
		openResult <- manager.Open(t.Context(), client, OpenRequest{
			ID: first.ID, Generation: first.Generation,
		})
	}()
	<-sendEntered
	revokeResult := make(chan error, 1)
	go func() {
		revokeResult <- manager.Revoke(t.Context(), Identity{
			ID: first.ID, Generation: first.Generation,
		})
	}()
	revokedBeforeOpened := false
	select {
	case err := <-revokeResult:
		if err != nil {
			t.Fatal(err)
		}
		revokedBeforeOpened = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseSend)
	<-openResult
	if !revokedBeforeOpened {
		if err := <-revokeResult; err != nil {
			t.Fatal(err)
		}
	}
	frame, ok := client.queue.pop(t.Context())
	if !ok || frame.tag != TagOpened {
		t.Fatalf("first concurrent viewer frame = %+v, %v; want opened", frame, ok)
	}
	manager.Disconnected(client)
	if err := manager.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestManagerDiagnosticFailureCategoryNeverLogsRawError(t *testing.T) {
	sink := &recordingDiagnosticSink{}
	server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
	manager, err := NewManager(ManagerOptions{
		Workspace:   "vb",
		Diagnostics: sink,
		StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
			return server, nil
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return nil, errors.New("SENTINEL raw tunnel error with credential and terminal output")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.Mirror(t.Context(), HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "private-session",
	})
	if err == nil {
		t.Fatal("mirror unexpectedly started")
	}
	records := sink.snapshot()
	if !containsDiagnostic(records, "tunnel", "start_failed", "tunnel_unavailable") {
		t.Fatalf("safe tunnel failure category missing: %+v", records)
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte("SENTINEL"), []byte("credential"), []byte("terminal output"), []byte("private-session")} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatalf("diagnostic records leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestManagerRecordsSafeViewerFailureCategory(t *testing.T) {
	sink := &recordingDiagnosticSink{}
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	manager, err := NewManager(ManagerOptions{
		Workspace:   "vb",
		Diagnostics: sink,
		Viewers: viewerFactoryFunc(func(
			context.Context, Identity, func([]byte) error,
		) (PreparedViewer, error) {
			return nil, errors.New("SENTINEL raw viewer error and terminal output")
		}),
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Shutdown(context.Background()) })
	session := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "private-session",
	}
	if err := manager.Mirror(t.Context(), session); err != nil {
		t.Fatal(err)
	}
	client := &Client{queue: newOutboundQueue(MaxClientQueueBytes)}
	manager.Connected(client)
	if _, ok := client.queue.pop(t.Context()); !ok {
		t.Fatal("client did not receive status")
	}
	openErr := manager.Open(t.Context(), client, OpenRequest{
		ID: session.ID, Generation: session.Generation,
	})
	manager.Disconnected(client)
	if openErr == nil {
		t.Fatal("viewer unexpectedly opened")
	}
	records := sink.snapshot()
	if !containsDiagnostic(records, "viewer", "open_failed", "terminal_unavailable") {
		t.Fatalf("safe viewer failure category missing: %+v", records)
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("SENTINEL")) || bytes.Contains(encoded, []byte("terminal output")) {
		t.Fatalf("viewer failure diagnostic leaked raw error: %s", encoded)
	}
}

func TestManagerCancelledStartCleansUpWithoutFailedState(t *testing.T) {
	server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
			return server, nil
		},
		StartTunnel: func(ctx context.Context, _ string) (TunnelResource, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err = manager.Mirror(ctx, HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mirror error = %v", err)
	}
	if server.closed.Load() != 1 {
		t.Fatalf("partial server closes = %d", server.closed.Load())
	}
	snapshot := manager.Snapshot()
	if snapshot.State != StateStopped || snapshot.Err != "" || len(snapshot.Sessions) != 0 {
		t.Fatalf("cancelled snapshot = %+v", snapshot)
	}
}

func TestManagerCancellationAtTunnelReadyDoesNotCommitMirror(t *testing.T) {
	server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	operationContext, cancelOperation := context.WithCancel(t.Context())
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
			return server, nil
		},
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			cancelOperation()
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = manager.Mirror(operationContext, HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("mirror error = %v", err)
	}
	snapshot := manager.Snapshot()
	if snapshot.State != StateStopped || len(snapshot.Sessions) != 0 {
		t.Fatalf("canceled ready-boundary snapshot = %+v", snapshot)
	}
	if server.closed.Load() != 1 || tunnel.closed.Load() != 1 {
		t.Fatalf(
			"canceled ready-boundary closes = server:%d tunnel:%d",
			server.closed.Load(),
			tunnel.closed.Load(),
		)
	}
}

func TestManagerShutdownCancelsBlockedStartupBeforeWaiting(t *testing.T) {
	server := &fakeServerResource{localURL: "http://127.0.0.1:43123"}
	tunnelStarted := make(chan struct{})
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		StartServer: func(context.Context, ServerOptions) (ServerResource, error) {
			return server, nil
		},
		StartTunnel: func(ctx context.Context, _ string) (TunnelResource, error) {
			close(tunnelStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	mirrorResult := make(chan error, 1)
	go func() {
		mirrorResult <- manager.Mirror(context.Background(), HostSession{
			ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
		})
	}()
	<-tunnelStarted

	shutdownContext, cancelShutdown := context.WithTimeout(t.Context(), time.Second)
	defer cancelShutdown()
	if err := manager.Shutdown(shutdownContext); err != nil {
		t.Fatal(err)
	}
	if err := receiveWithin(t, mirrorResult, "canceled mirror startup"); !errors.Is(err, context.Canceled) {
		t.Fatalf("mirror startup error = %v", err)
	}
	if got := manager.Snapshot().State; got != StateStopped {
		t.Fatalf("state after shutdown during startup = %v", got)
	}
}

func TestManagerEventQueueCoalescesSnapshotsBeforeDroppingViewedEvent(t *testing.T) {
	manager, err := NewManager(ManagerOptions{Workspace: "vb"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < cap(manager.events); i++ {
		manager.publishSnapshot(Snapshot{State: StateStarting, Err: strings.Repeat("x", i)})
	}
	viewed := Event{Viewed: &ViewedEvent{
		ID: "$7", Generation: "generation-a", Activity: 42,
	}}
	if !manager.publishViewed(viewed) {
		t.Fatal("viewed event was dropped behind coalescible snapshots")
	}
	found := false
	for len(manager.events) > 0 {
		event := <-manager.events
		if event.Viewed != nil && event.Viewed.Activity == 42 {
			found = true
		}
	}
	if !found {
		t.Fatal("viewed event was not queued")
	}
}

func TestManagerEventQueueCoalescesSnapshotsBehindViewedEvent(t *testing.T) {
	manager, err := NewManager(ManagerOptions{Workspace: "vb"})
	if err != nil {
		t.Fatal(err)
	}
	firstViewed := Event{Viewed: &ViewedEvent{
		ID: "$7", Generation: "generation-a", Activity: 41,
	}}
	manager.events <- firstViewed
	for i := 1; i < cap(manager.events); i++ {
		manager.events <- Event{Snapshot: &Snapshot{
			State: StateStarting,
			Err:   strings.Repeat("x", i),
		}}
	}
	secondViewed := Event{Viewed: &ViewedEvent{
		ID: "$8", Generation: "generation-a", Activity: 42,
	}}
	if !manager.publishViewed(secondViewed) {
		t.Fatal("viewed event was dropped behind an older viewed event and snapshots")
	}
	var activities []int64
	for len(manager.events) > 0 {
		event := <-manager.events
		if event.Viewed != nil {
			activities = append(activities, event.Viewed.Activity)
		}
	}
	if len(activities) != 2 || activities[0] != 41 || activities[1] != 42 {
		t.Fatalf("viewed activity events = %v", activities)
	}
}

func TestManagerSnapshotCoalescesBehindViewedEvent(t *testing.T) {
	manager, err := NewManager(ManagerOptions{Workspace: "vb"})
	if err != nil {
		t.Fatal(err)
	}
	manager.events <- Event{Viewed: &ViewedEvent{
		ID: "$7", Generation: "generation-a", Activity: 41,
	}}
	for i := 1; i < cap(manager.events); i++ {
		manager.events <- Event{Snapshot: &Snapshot{
			State: StateStarting,
			Err:   strings.Repeat("x", i),
		}}
	}
	manager.publishSnapshot(Snapshot{State: StateReady, Err: "latest"})
	var foundViewed, foundLatest bool
	for len(manager.events) > 0 {
		event := <-manager.events
		foundViewed = foundViewed || event.Viewed != nil
		foundLatest = foundLatest ||
			event.Snapshot != nil && event.Snapshot.State == StateReady && event.Snapshot.Err == "latest"
	}
	if !foundViewed || !foundLatest {
		t.Fatalf("coalesced events viewed/latest = %v/%v", foundViewed, foundLatest)
	}
}

func TestManagerViewerExitClosesActiveSessionAndNotifiesClient(t *testing.T) {
	viewer := &fakeViewer{done: make(chan error, 1)}
	tunnel := &fakeTunnelResource{
		url:  "https://quiet-river.trycloudflare.com",
		done: make(chan error),
	}
	manager, err := NewManager(ManagerOptions{
		Workspace: "vb",
		Viewers: viewerFactoryFunc(func(
			context.Context,
			Identity,
			func([]byte) error,
		) (PreparedViewer, error) {
			return prepareFakeViewer(viewer), nil
		}),
		StartTunnel: func(context.Context, string) (TunnelResource, error) {
			return tunnel, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := HostSession{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb/api",
	}
	if err := manager.Mirror(t.Context(), session); err != nil {
		t.Fatal(err)
	}

	client := &Client{queue: newOutboundQueue(MaxClientQueueBytes)}
	manager.Connected(client)
	if _, ok := client.queue.pop(t.Context()); !ok {
		t.Fatal("client did not receive initial status")
	}
	if err := manager.Open(t.Context(), client, OpenRequest{
		ID: session.ID, Generation: session.Generation,
	}); err != nil {
		t.Fatal(err)
	}
	if frame, ok := client.queue.pop(t.Context()); !ok || frame.tag != TagOpened {
		t.Fatalf("client did not receive opened geometry: %+v, %v", frame, ok)
	}
	client.viewerOpen.Store(true)

	viewer.done <- nil
	close(viewer.done)
	frameContext, cancelFrame := context.WithTimeout(t.Context(), time.Second)
	defer cancelFrame()
	frame, ok := client.queue.pop(frameContext)
	if !ok {
		t.Fatal("viewer exit did not notify the client")
	}
	if frame.tag != TagError {
		t.Fatalf("viewer exit frame tag = 0x%02x, want 0x%02x", frame.tag, TagError)
	}
	var problem ProtocolError
	if err := DecodeControl(frame.tag, frame.payload, &problem); err != nil {
		t.Fatal(err)
	}
	if problem.Code != "terminal_ended" || problem.Message != "The terminal viewer ended." || !problem.Retry {
		t.Fatalf("viewer exit problem = %+v", problem)
	}
	if client.viewerOpen.Load() {
		t.Fatal("viewer exit left the client terminal marked open")
	}
	if err := manager.Input(client, []byte("ignored")); err == nil {
		t.Fatal("viewer exit left an active manager session")
	}

	manager.Disconnected(client)
	if err := manager.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
}
