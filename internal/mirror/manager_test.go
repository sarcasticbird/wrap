package mirror

import (
	"bytes"
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
	onSecret   func()
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
	hook := s.onSecret
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
}
func (s *fakeServerResource) Close(context.Context) error {
	s.closed.Add(1)
	return nil
}

type viewerFactoryFunc func(context.Context, Identity, func([]byte) error) (PreparedViewer, error)

func (f viewerFactoryFunc) Prepare(ctx context.Context, identity Identity, output func([]byte) error) (PreparedViewer, error) {
	return f(ctx, identity, output)
}

type trackingCleanupViewerFactory struct {
	cleanupErr error
	cleaned    atomic.Int32
}

func (*trackingCleanupViewerFactory) Prepare(context.Context, Identity, func([]byte) error) (PreparedViewer, error) {
	return nil, errors.New("not used")
}

func (f *trackingCleanupViewerFactory) Cleanup() error {
	f.cleaned.Add(1)
	return f.cleanupErr
}

type fakePreparedViewer struct {
	geometry ViewerGeometry
	viewer   Viewer
	startErr error
	closed   atomic.Int32
}

func (v *fakePreparedViewer) Geometry() ViewerGeometry { return v.geometry }
func (v *fakePreparedViewer) Start() (Viewer, error) {
	if v.startErr != nil {
		return nil, v.startErr
	}
	return v.viewer, nil
}
func (v *fakePreparedViewer) Close() error {
	v.closed.Add(1)
	return nil
}

type fakeViewer struct {
	done     chan error
	closeErr error
	closed   atomic.Int32
	writes   atomic.Int32
}

func newFakeViewer() *fakeViewer { return &fakeViewer{done: make(chan error, 1)} }
func (v *fakeViewer) Write([]byte) error {
	v.writes.Add(1)
	return nil
}
func (v *fakeViewer) Close() error {
	v.closed.Add(1)
	return v.closeErr
}
func (v *fakeViewer) Done() <-chan error { return v.done }

type blockingErrorViewer struct {
	done    chan error
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBlockingErrorViewer() *blockingErrorViewer {
	return &blockingErrorViewer{
		done: make(chan error, 1), started: make(chan struct{}), release: make(chan struct{}),
	}
}

func (*blockingErrorViewer) Write([]byte) error { return nil }
func (v *blockingErrorViewer) Close() error {
	v.once.Do(func() { close(v.started) })
	<-v.release
	return errors.New("viewer cleanup failed")
}
func (v *blockingErrorViewer) Done() <-chan error { return v.done }

func managerTarget() HostSession {
	return HostSession{ID: "$7", WindowID: "@3", Generation: "generation", Name: "api", Kind: "terminal"}
}

func newReadyManager(t *testing.T, viewers ViewerFactory) (*Manager, *fakeServerResource, *fakeTunnelResource) {
	t.Helper()
	server := &fakeServerResource{localURL: "http://127.0.0.1:1234"}
	tunnel := &fakeTunnelResource{url: "https://quiet-river.trycloudflare.com", done: make(chan error, 1)}
	manager, err := NewManager(ManagerOptions{
		Workspace: "api",
		Target:    ptr(managerTarget()),
		Viewers:   viewers,
		Random: bytes.NewReader(append(
			bytes.Repeat([]byte{0x42}, 32),
			bytes.Repeat([]byte{0x43}, 64)...,
		)),
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
	if err := manager.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return manager, server, tunnel
}

func ptr[T any](value T) *T { return &value }

func TestManagerRequiresExactlyOneTarget(t *testing.T) {
	if _, err := NewManager(ManagerOptions{Workspace: "api"}); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("NewManager without target = %v", err)
	}
}

func TestManagerStartOwnsOneServerTunnelAndTarget(t *testing.T) {
	manager, server, tunnel := newReadyManager(t, viewerFactoryFunc(func(context.Context, Identity, func([]byte) error) (PreparedViewer, error) {
		return &fakePreparedViewer{geometry: ViewerGeometry{Columns: 80, Rows: 24}, viewer: newFakeViewer()}, nil
	}))
	if snapshot := manager.Snapshot(); snapshot.State != StateReady || snapshot.PairingURL == "" || snapshot.QR == "" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if err := manager.Start(t.Context()); err != nil {
		t.Fatalf("idempotent Start = %v", err)
	}
	if err := manager.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if server.closed.Load() != 1 || tunnel.closed.Load() != 1 {
		t.Fatalf("resource closes = server:%d tunnel:%d", server.closed.Load(), tunnel.closed.Load())
	}
}

func TestManagerShutdownSurfacesDeferredViewerFactoryCleanupFailure(t *testing.T) {
	factory := &trackingCleanupViewerFactory{cleanupErr: errors.New("restore mirrored tmux window size: tmux unavailable")}
	manager, _, _ := newReadyManager(t, factory)
	err := manager.Shutdown(t.Context())
	if err == nil || !strings.Contains(err.Error(), "tmux unavailable") {
		t.Fatalf("Shutdown() = %v, want viewer cleanup error", err)
	}
	if factory.cleaned.Load() != 1 {
		t.Fatalf("viewer factory cleanup calls = %d, want 1", factory.cleaned.Load())
	}
}

func TestManagerConnectedAutomaticallyOpensSoleTarget(t *testing.T) {
	var preparedIdentity Identity
	viewer := newFakeViewer()
	manager, _, _ := newReadyManager(t, viewerFactoryFunc(func(_ context.Context, identity Identity, _ func([]byte) error) (PreparedViewer, error) {
		preparedIdentity = identity
		return &fakePreparedViewer{geometry: ViewerGeometry{Columns: 120, Rows: 40}, viewer: viewer}, nil
	}))
	client := &Client{handler: manager, queue: newOutboundQueue(1 << 20)}
	if err := manager.Connected(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	frame, ok := client.queue.pop(t.Context())
	if !ok || frame.tag != TagReady {
		t.Fatalf("first frame = %+v, ok=%v", frame, ok)
	}
	var ready Ready
	if err := DecodeControl(frame.tag, frame.payload, &ready); err != nil {
		t.Fatal(err)
	}
	if preparedIdentity.ID != managerTarget().ID || preparedIdentity.WindowID != managerTarget().WindowID || ready.Columns != 120 {
		t.Fatalf("identity/ready = %+v / %+v", preparedIdentity, ready)
	}
	if manager.ClientCount() != 1 {
		t.Fatalf("clients = %d", manager.ClientCount())
	}
	manager.Disconnected(client)
	if manager.ClientCount() != 0 || viewer.closed.Load() != 1 {
		t.Fatalf("disconnect = clients:%d viewer closes:%d", manager.ClientCount(), viewer.closed.Load())
	}
}

func TestManagerDisconnectSurfacesViewerCleanupFailure(t *testing.T) {
	viewer := newFakeViewer()
	viewer.closeErr = errors.New("restore mirrored tmux window size: tmux unavailable")
	manager, _, _ := newReadyManager(t, viewerFactoryFunc(func(context.Context, Identity, func([]byte) error) (PreparedViewer, error) {
		return &fakePreparedViewer{geometry: ViewerGeometry{Columns: 80, Rows: 24}, viewer: viewer}, nil
	}))
	client := &Client{handler: manager, queue: newOutboundQueue(1 << 20)}
	if err := manager.Connected(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	_, _ = client.queue.pop(t.Context())
	manager.Disconnected(client)
	if warning := manager.Snapshot().CleanupWarning; warning == "" {
		t.Fatal("disconnect cleanup failure was not surfaced")
	}
}

func TestManagerUnexpectedViewerErrorSurfacesCleanupWarning(t *testing.T) {
	viewer := newFakeViewer()
	manager, _, _ := newReadyManager(t, viewerFactoryFunc(func(context.Context, Identity, func([]byte) error) (PreparedViewer, error) {
		return &fakePreparedViewer{geometry: ViewerGeometry{Columns: 80, Rows: 24}, viewer: viewer}, nil
	}))
	client := &Client{handler: manager, queue: newOutboundQueue(1 << 20)}
	if err := manager.Connected(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	_, _ = client.queue.pop(t.Context())
	viewer.closeErr = errors.New("restore mirrored tmux window size: tmux unavailable")
	viewer.done <- viewer.closeErr
	deadline := time.Now().Add(time.Second)
	for manager.Snapshot().CleanupWarning == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if warning := manager.Snapshot().CleanupWarning; warning == "" {
		t.Fatal("unexpected viewer cleanup failure was not surfaced")
	}
}

func TestManagerRotateCommitsSecretBeforeCleanupAndReturnsSuccess(t *testing.T) {
	viewer := newFakeViewer()
	viewer.closeErr = errors.New("viewer cleanup failed")
	manager, server, _ := newReadyManager(t, viewerFactoryFunc(func(context.Context, Identity, func([]byte) error) (PreparedViewer, error) {
		return &fakePreparedViewer{geometry: ViewerGeometry{Columns: 80, Rows: 24}, viewer: viewer}, nil
	}))
	client := &Client{handler: manager, queue: newOutboundQueue(1 << 20)}
	if err := manager.Connected(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	_, _ = client.queue.pop(t.Context())
	// This unit client has no WebSocket. Keep its active viewer in the manager
	// while excluding it from WebSocket cleanup, which is covered by server tests.
	manager.mu.Lock()
	delete(manager.clients, client)
	manager.mu.Unlock()
	server.onSecret = func() {
		if viewer.closed.Load() != 0 {
			t.Error("viewer cleanup ran before credential cutoff")
		}
	}
	oldURL := manager.Snapshot().PairingURL
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if err := manager.Rotate(ctx); err != nil {
		t.Fatalf("Rotate returned post-commit cleanup error: %v", err)
	}
	if got := manager.Snapshot().PairingURL; got == oldURL || got == "" {
		t.Fatalf("pairing URL after rotate = %q", got)
	}
	if got := manager.Snapshot().CleanupWarning; got == "" {
		t.Fatal("rotation cleanup failure was not surfaced")
	}
	server.mu.Lock()
	rotations := len(server.secrets)
	server.mu.Unlock()
	if rotations != 1 || viewer.closed.Load() == 0 {
		t.Fatalf("rotation/cleanup = %d/%d", rotations, viewer.closed.Load())
	}
}

func TestManagerRotateDoesNotWarnForAlreadyRevokedClients(t *testing.T) {
	viewer := newFakeViewer()
	manager, _, _ := newReadyManager(t, viewerFactoryFunc(func(context.Context, Identity, func([]byte) error) (PreparedViewer, error) {
		return &fakePreparedViewer{geometry: ViewerGeometry{Columns: 80, Rows: 24}, viewer: viewer}, nil
	}))
	client := &Client{handler: manager, queue: newOutboundQueue(1 << 20)}
	if err := manager.Connected(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	_, _ = client.queue.pop(t.Context())
	client.closeOnce.Do(func() {})
	client.queue.close(errors.New("mirror pairing credential rotated"))
	if err := manager.Rotate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if warning := manager.Snapshot().CleanupWarning; warning != "" {
		t.Fatalf("expected credential revocation reported as cleanup failure: %q", warning)
	}
	if viewer.closed.Load() != 1 {
		t.Fatalf("viewer closes = %d", viewer.closed.Load())
	}
}

func TestManagerRotateOwnsViewerCleanupBeforeDisconnect(t *testing.T) {
	viewer := newBlockingErrorViewer()
	manager, server, _ := newReadyManager(t, viewerFactoryFunc(func(context.Context, Identity, func([]byte) error) (PreparedViewer, error) {
		return &fakePreparedViewer{geometry: ViewerGeometry{Columns: 80, Rows: 24}, viewer: viewer}, nil
	}))
	client := &Client{handler: manager, queue: newOutboundQueue(1 << 20)}
	if err := manager.Connected(t.Context(), client); err != nil {
		t.Fatal(err)
	}
	_, _ = client.queue.pop(t.Context())
	manager.mu.Lock()
	delete(manager.clients, client)
	manager.mu.Unlock()
	secretEntered := make(chan struct{})
	allowSecret := make(chan struct{})
	server.onSecret = func() {
		close(secretEntered)
		<-allowSecret
	}
	rotateDone := make(chan error, 1)
	go func() { rotateDone <- manager.Rotate(t.Context()) }()
	<-secretEntered
	disconnectDone := make(chan struct{})
	go func() {
		manager.Disconnected(client)
		close(disconnectDone)
	}()
	closedBeforeCutoffReturned := false
	select {
	case <-viewer.started:
		closedBeforeCutoffReturned = true
		close(viewer.release)
		<-disconnectDone
	case <-time.After(50 * time.Millisecond):
	}
	close(allowSecret)
	if !closedBeforeCutoffReturned {
		select {
		case <-viewer.started:
			close(viewer.release)
		case <-time.After(time.Second):
			t.Fatal("rotation never closed detached viewer")
		}
	}
	if err := <-rotateDone; err != nil {
		t.Fatal(err)
	}
	<-disconnectDone
	if warning := manager.Snapshot().CleanupWarning; warning == "" {
		t.Fatal("disconnect race hid viewer cleanup failure")
	}
}

func TestManagerUnexpectedTunnelExitFailsClosed(t *testing.T) {
	manager, server, tunnel := newReadyManager(t, viewerFactoryFunc(func(context.Context, Identity, func([]byte) error) (PreparedViewer, error) {
		return &fakePreparedViewer{geometry: ViewerGeometry{Columns: 80, Rows: 24}, viewer: newFakeViewer()}, nil
	}))
	tunnel.done <- errors.New("tunnel exited")
	deadline := time.Now().Add(time.Second)
	for (manager.Snapshot().State != StateFailed || server.closed.Load() != 1) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if snapshot := manager.Snapshot(); snapshot.State != StateFailed || snapshot.PairingURL != "" {
		t.Fatalf("failed snapshot = %+v", snapshot)
	}
	if server.closed.Load() != 1 {
		t.Fatalf("server closes = %d", server.closed.Load())
	}
}
