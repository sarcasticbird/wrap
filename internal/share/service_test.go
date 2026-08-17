package share

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarcasticbird/wrap/internal/control"
	"github.com/sarcasticbird/wrap/internal/instance"
	"github.com/sarcasticbird/wrap/internal/mirror"
	"github.com/sarcasticbird/wrap/internal/target"
)

type fakeHelper struct {
	sessionID string
	closed    atomic.Int32
	validate  func(context.Context) error
	onClose   func()
	closeErr  error
}

func (h *fakeHelper) ID() string { return h.sessionID }
func (h *fakeHelper) Validate(ctx context.Context) error {
	if h.validate != nil {
		return h.validate(ctx)
	}
	return nil
}
func (h *fakeHelper) Close() error {
	if h.onClose != nil {
		h.onClose()
	}
	h.closed.Add(1)
	return h.closeErr
}

type fakeMirror struct {
	mu          sync.Mutex
	snapshot    mirror.Snapshot
	events      chan mirror.Event
	startErr    error
	starts      atomic.Int32
	rotations   atomic.Int32
	shutdowns   atomic.Int32
	clientCount int
	onShutdown  func()
}

type failingDiagnosticSink struct{}

func (failingDiagnosticSink) Write(mirror.DiagnosticRecord) error {
	return errors.New("diagnostic disk unavailable")
}

type statusServerResource struct{}

func (statusServerResource) LocalURL() string            { return "http://127.0.0.1:1234" }
func (statusServerResource) SetPublicHost(string)        {}
func (statusServerResource) SetSecret(mirror.Secret)     {}
func (statusServerResource) Close(context.Context) error { return nil }

type statusTunnelResource struct {
	done chan error
}

func (statusTunnelResource) URL() string                 { return "https://quiet-river.trycloudflare.com" }
func (resource statusTunnelResource) Done() <-chan error { return resource.done }
func (statusTunnelResource) Close() error                { return nil }

func newFakeMirror() *fakeMirror {
	return &fakeMirror{
		snapshot: mirror.Snapshot{
			State:      mirror.StateReady,
			PairingURL: "https://quiet-river.trycloudflare.com/#k=secret",
			QR:         "qr",
		},
		events: make(chan mirror.Event, 4),
	}
}

func (m *fakeMirror) Start(context.Context) error {
	m.starts.Add(1)
	return m.startErr
}
func (m *fakeMirror) Snapshot() mirror.Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshot
}
func (m *fakeMirror) Rotate(context.Context) error {
	m.rotations.Add(1)
	m.mu.Lock()
	m.snapshot.PairingURL = "https://quiet-river.trycloudflare.com/#k=rotated"
	m.snapshot.CleanupWarning = "pairing rotated, but terminal cleanup was incomplete"
	m.mu.Unlock()
	return nil
}
func (m *fakeMirror) Shutdown(context.Context) error {
	if m.onShutdown != nil {
		m.onShutdown()
	}
	m.shutdowns.Add(1)
	return nil
}
func (m *fakeMirror) Events() <-chan mirror.Event { return m.events }
func (m *fakeMirror) ClientCount() int            { return m.clientCount }

func shareTestRecord(t *testing.T) (instance.Record, instance.Store) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "wrap-share-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	record := instance.Record{
		Version:       instance.RecordVersion,
		ID:            "01KWRAPSHARE",
		Name:          "api",
		PID:           os.Getpid(),
		ControlSocket: filepath.Join(root, "runtime", "01KWRAPSHARE.sock"),
		StartedAt:     time.Unix(100, 0).UTC(),
		Directory:     "/work/api",
		Target: target.Target{
			SocketPath: "/tmp/tmux.sock",
			Generation: "0123456789abcdef0123456789abcdef",
			SessionID:  "$1",
			WindowID:   "@2",
			Directory:  "/work/api",
		},
	}
	return record, instance.Store{
		StateRoot:   filepath.Join(root, "state"),
		RuntimeRoot: filepath.Join(root, "runtime"),
	}
}

func TestServiceReadyControlAndShutdownLifecycle(t *testing.T) {
	record, store := shareTestRecord(t)
	helper := &fakeHelper{sessionID: "$9"}
	mirrorService := newFakeMirror()
	assertRecordPublished := func() {
		if _, err := store.Resolve(record.ID); err != nil {
			t.Errorf("instance record disappeared before resource cleanup: %v", err)
		}
	}
	helper.onClose = assertRecordPublished
	mirrorService.onShutdown = assertRecordPublished
	ready := make(chan control.Status, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			Record:       record,
			Store:        store,
			CreateHelper: func(target.Target, string) (Helper, error) { return helper, nil },
			NewMirror:    func(Helper, instance.Record) (Mirror, error) { return mirrorService, nil },
			Ready:        func(status control.Status) error { ready <- status; return nil },
		})
	}()

	select {
	case status := <-ready:
		if status.ID != record.ID || status.PairingURL == "" || status.Target.WindowID != record.Target.WindowID {
			t.Fatalf("ready status = %#v", status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not become ready")
	}
	records, problems, err := store.ReadAll()
	if err != nil || len(problems) != 0 || len(records) != 1 {
		t.Fatalf("records after readiness = %#v, problems=%v, err=%v", records, problems, err)
	}
	if lease, err := store.AcquireLease(record.ID); !errors.Is(err, instance.ErrLeaseHeld) || lease != nil {
		t.Fatalf("worker lease while live = %v, %v", lease, err)
	}
	renamed, err := control.Call(t.Context(), record.ControlSocket, control.Request{
		InstanceID: record.ID, Action: control.ActionRename, Name: "api two",
	})
	if err != nil || renamed.Name != "api two" {
		t.Fatalf("renamed status = %+v, %v", renamed, err)
	}
	if stored, err := store.Resolve("api two"); err != nil || stored.ID != record.ID {
		t.Fatalf("renamed record = %+v, %v", stored, err)
	}

	rotated, err := control.Call(t.Context(), record.ControlSocket, control.Request{
		InstanceID: record.ID,
		Action:     control.ActionRotate,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(rotated.PairingURL, "#k=rotated") || rotated.Warning == "" || mirrorService.rotations.Load() != 1 {
		t.Fatalf("rotated status = %#v", rotated)
	}
	if _, err := control.Call(t.Context(), record.ControlSocket, control.Request{
		InstanceID: record.ID,
		Action:     control.ActionShutdown,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not stop")
	}
	if helper.closed.Load() != 1 || mirrorService.shutdowns.Load() != 1 {
		t.Fatalf("cleanup helper/manager = %d/%d", helper.closed.Load(), mirrorService.shutdowns.Load())
	}
	records, _, err = store.ReadAll()
	if err != nil || len(records) != 0 {
		t.Fatalf("records after shutdown = %#v, err=%v", records, err)
	}
	lease, err := store.AcquireLease(record.ID)
	if err != nil {
		t.Fatalf("worker lease after shutdown = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestServiceStatusPropagatesFailingDiagnosticSink(t *testing.T) {
	record, store := shareTestRecord(t)
	manager, err := mirror.NewManager(mirror.ManagerOptions{
		Workspace: "api",
		Target: &mirror.HostSession{
			ID: record.Target.SessionID, WindowID: record.Target.WindowID,
			Generation: record.Target.Generation, Name: record.Name, Kind: "terminal",
		},
		Diagnostics: failingDiagnosticSink{},
		StartServer: func(context.Context, mirror.ServerOptions) (mirror.ServerResource, error) {
			return statusServerResource{}, nil
		},
		StartTunnel: func(context.Context, string) (mirror.TunnelResource, error) {
			return statusTunnelResource{done: make(chan error)}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stopAfterReady := errors.New("stop after readiness")
	var ready control.Status
	err = Run(t.Context(), Options{
		Record:       record,
		Store:        store,
		CreateHelper: func(target.Target, string) (Helper, error) { return &fakeHelper{sessionID: "$9"}, nil },
		NewMirror:    func(Helper, instance.Record) (Mirror, error) { return manager, nil },
		Ready: func(status control.Status) error {
			ready = status
			return stopAfterReady
		},
	})
	if !errors.Is(err, stopAfterReady) {
		t.Fatalf("Run() = %v", err)
	}
	if !strings.Contains(ready.Warning, "diagnostics unavailable") {
		t.Fatalf("ready warning = %q", ready.Warning)
	}
}

func TestServiceStatusCombinesDiagnosticAndCleanupWarnings(t *testing.T) {
	record, store := shareTestRecord(t)
	mirrorService := newFakeMirror()
	mirrorService.snapshot.DiagnosticsWarning = "diagnostics unavailable"
	mirrorService.snapshot.CleanupWarning = "terminal cleanup was incomplete"
	handler := &serviceHandler{record: record, store: store, mirror: mirrorService}
	status := handler.Status()
	if status.Warning != "diagnostics unavailable; terminal cleanup was incomplete" {
		t.Fatalf("status warning = %q", status.Warning)
	}
}

func TestServiceKeepsRecordAndLeaseUntilResourceCleanupFinishes(t *testing.T) {
	record, store := shareTestRecord(t)
	helper := &fakeHelper{sessionID: "$9"}
	mirrorService := newFakeMirror()
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	mirrorService.onShutdown = func() {
		close(cleanupStarted)
		<-releaseCleanup
	}
	ready := make(chan control.Status, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			Record: record, Store: store,
			CreateHelper: func(target.Target, string) (Helper, error) { return helper, nil },
			NewMirror:    func(Helper, instance.Record) (Mirror, error) { return mirrorService, nil },
			Ready:        func(status control.Status) error { ready <- status; return nil },
		})
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("service did not become ready")
	}
	if _, err := control.Call(t.Context(), record.ControlSocket, control.Request{
		InstanceID: record.ID, Action: control.ActionShutdown,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-cleanupStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("resource cleanup did not start")
	}
	if _, err := store.Resolve(record.ID); err != nil {
		t.Fatalf("record disappeared during cleanup: %v", err)
	}
	if held, err := store.LeaseHeld(record.ID); err != nil || !held {
		t.Fatalf("lease during cleanup = %v, %v", held, err)
	}
	close(releaseCleanup)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not finish cleanup")
	}
	if _, err := store.Resolve(record.ID); !errors.Is(err, instance.ErrNotFound) {
		t.Fatalf("record after cleanup = %v", err)
	}
	if held, err := store.LeaseHeld(record.ID); err != nil || held {
		t.Fatalf("lease after cleanup = %v, %v", held, err)
	}
}

func TestServiceRetainsRecordWhenResourceCleanupFails(t *testing.T) {
	record, store := shareTestRecord(t)
	helper := &fakeHelper{sessionID: "$9", closeErr: errors.New("tmux temporarily unavailable")}
	mirrorService := newFakeMirror()
	ready := make(chan control.Status, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			Record: record, Store: store,
			CreateHelper: func(target.Target, string) (Helper, error) { return helper, nil },
			NewMirror:    func(Helper, instance.Record) (Mirror, error) { return mirrorService, nil },
			Ready:        func(status control.Status) error { ready <- status; return nil },
		})
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("service did not become ready")
	}
	if _, err := control.Call(t.Context(), record.ControlSocket, control.Request{
		InstanceID: record.ID, Action: control.ActionShutdown,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "tmux temporarily unavailable") {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not finish cleanup")
	}
	if stored, err := store.Resolve(record.ID); err != nil || stored.ID != record.ID {
		t.Fatalf("record after failed cleanup = %+v, %v", stored, err)
	}
	if held, err := store.LeaseHeld(record.ID); err != nil || held {
		t.Fatalf("lease after failed cleanup = %v, %v", held, err)
	}
}

func TestServiceCleansPartialStartWithoutPublishingRecord(t *testing.T) {
	record, store := shareTestRecord(t)
	helper := &fakeHelper{sessionID: "$9"}
	mirrorService := newFakeMirror()
	mirrorService.startErr = errors.New("tunnel failed")
	err := Run(t.Context(), Options{
		Record:       record,
		Store:        store,
		CreateHelper: func(target.Target, string) (Helper, error) { return helper, nil },
		NewMirror:    func(Helper, instance.Record) (Mirror, error) { return mirrorService, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "tunnel failed") {
		t.Fatalf("Run() = %v", err)
	}
	if helper.closed.Load() != 1 || mirrorService.shutdowns.Load() != 1 {
		t.Fatalf("partial cleanup helper/manager = %d/%d", helper.closed.Load(), mirrorService.shutdowns.Load())
	}
	records, _, readErr := store.ReadAll()
	if readErr != nil || len(records) != 0 {
		t.Fatalf("records after failure = %#v, err=%v", records, readErr)
	}
}

func TestServiceStopsWhenTargetDisappears(t *testing.T) {
	record, store := shareTestRecord(t)
	vanished := make(chan struct{})
	helper := &fakeHelper{sessionID: "$9", validate: func(context.Context) error {
		select {
		case <-vanished:
			return errors.New("captured tmux window disappeared")
		default:
			return nil
		}
	}}
	mirrorService := newFakeMirror()
	ready := make(chan control.Status, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Options{
			Record:           record,
			Store:            store,
			CreateHelper:     func(target.Target, string) (Helper, error) { return helper, nil },
			NewMirror:        func(Helper, instance.Record) (Mirror, error) { return mirrorService, nil },
			Ready:            func(status control.Status) error { ready <- status; return nil },
			ValidateInterval: time.Millisecond,
		})
	}()
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("service did not become ready")
	}
	close(vanished)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "captured tmux window disappeared") {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not notice vanished target")
	}
}

func TestServiceBoundsHelperValidationAndHonorsCancellation(t *testing.T) {
	record, store := shareTestRecord(t)
	previous := helperValidationTimeout
	helperValidationTimeout = 20 * time.Millisecond
	t.Cleanup(func() { helperValidationTimeout = previous })
	helper := &fakeHelper{sessionID: "$9", validate: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}
	mirrorService := newFakeMirror()
	started := time.Now()
	err := Run(t.Context(), Options{
		Record:           record,
		Store:            store,
		CreateHelper:     func(target.Target, string) (Helper, error) { return helper, nil },
		NewMirror:        func(Helper, instance.Record) (Mirror, error) { return mirrorService, nil },
		ValidateInterval: time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
		t.Fatalf("Run() = %v after %s", err, time.Since(started))
	}
}

func TestServiceStopsOnMirrorFailureAndContextCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		stop func(context.CancelFunc, *fakeMirror)
		want string
	}{
		{name: "mirror failure", stop: func(_ context.CancelFunc, service *fakeMirror) {
			service.events <- mirror.Event{Snapshot: &mirror.Snapshot{State: mirror.StateFailed, Err: "tunnel exited"}}
		}, want: "tunnel exited"},
		{name: "context cancellation", stop: func(cancel context.CancelFunc, _ *fakeMirror) { cancel() }, want: "context canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			record, store := shareTestRecord(t)
			helper := &fakeHelper{sessionID: "$9"}
			mirrorService := newFakeMirror()
			ready := make(chan control.Status, 1)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, Options{
					Record: record, Store: store,
					CreateHelper: func(target.Target, string) (Helper, error) { return helper, nil },
					NewMirror:    func(Helper, instance.Record) (Mirror, error) { return mirrorService, nil },
					Ready:        func(status control.Status) error { ready <- status; return nil },
				})
			}()
			select {
			case <-ready:
			case <-time.After(2 * time.Second):
				t.Fatal("service did not become ready")
			}
			test.stop(cancel, mirrorService)
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("Run() = %v, want %q", err, test.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("service did not stop")
			}
		})
	}
}
