package mirror

import (
	"context"
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
	uint16,
	uint16,
	func([]byte) error,
) (Viewer, error)

func (f viewerFactoryFunc) Open(
	ctx context.Context,
	identity Identity,
	columns, rows uint16,
	output func([]byte) error,
) (Viewer, error) {
	return f(ctx, identity, columns, rows, output)
}

type fakeViewer struct {
	done chan error
}

func (v *fakeViewer) Write([]byte) error          { return nil }
func (v *fakeViewer) Resize(uint16, uint16) error { return nil }
func (v *fakeViewer) Close() error                { return nil }
func (v *fakeViewer) Done() <-chan error          { return v.done }

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
			uint16,
			uint16,
			func([]byte) error,
		) (Viewer, error) {
			return viewer, nil
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
		ID: session.ID, Generation: session.Generation, Columns: 80, Rows: 24,
	}); err != nil {
		t.Fatal(err)
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
