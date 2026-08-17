package share

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sarcasticbird/wrap/internal/control"
	"github.com/sarcasticbird/wrap/internal/instance"
	"github.com/sarcasticbird/wrap/internal/mirror"
	"github.com/sarcasticbird/wrap/internal/target"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

const (
	defaultValidateInterval    = 2 * time.Second
	completedArtifactRetention = 32
)

var helperValidationTimeout = time.Second

type Helper interface {
	ID() string
	Validate(context.Context) error
	Close() error
}

type Mirror interface {
	Start(context.Context) error
	Snapshot() mirror.Snapshot
	Rotate(context.Context) error
	Shutdown(context.Context) error
	Events() <-chan mirror.Event
	ClientCount() int
}

type Options struct {
	Record instance.Record
	Store  instance.Store

	CreateHelper func(target.Target, string) (Helper, error)
	NewMirror    func(Helper, instance.Record) (Mirror, error)
	ServeControl func(context.Context, string, string, control.Handler) error
	Ready        func(control.Status) error

	StartServer func(context.Context, mirror.ServerOptions) (mirror.ServerResource, error)
	StartTunnel func(context.Context, string) (mirror.TunnelResource, error)
	Random      io.Reader
	Diagnostics mirror.DiagnosticSink
	TmuxRunner  tmux.Runner

	ValidateInterval time.Duration
}

type helperResource struct {
	helper *target.Helper
}

func (h helperResource) ID() string                         { return h.helper.ID() }
func (h helperResource) Validate(ctx context.Context) error { return h.helper.Validate(ctx) }
func (h helperResource) Close() error                       { return h.helper.Close() }

func Run(ctx context.Context, options Options) (retErr error) {
	if ctx == nil {
		return errors.New("share context is required")
	}
	record := options.Record
	record.PID = os.Getpid()
	if err := record.Validate(); err != nil {
		return fmt.Errorf("validate Wrap instance: %w", err)
	}
	if options.Store.StateRoot == "" {
		return errors.New("wrap instance store is required")
	}
	workerStore, err := options.Store.ForRecord(record)
	if err != nil {
		return fmt.Errorf("resolve Wrap worker runtime: %w", err)
	}
	lease, err := workerStore.AcquireLease(record.ID)
	if err != nil {
		return fmt.Errorf("acquire Wrap worker lease: %w", err)
	}
	published := false
	var cleanupErr error
	joinCleanupError := func(err error) {
		cleanupErr = errors.Join(cleanupErr, err)
		retErr = errors.Join(retErr, err)
	}
	defer func() {
		if !published || cleanupErr != nil {
			return
		}
		_, err := options.Store.RemoveIfPID(record.ID, record.PID)
		retErr = errors.Join(retErr, err)
	}()
	defer func() { joinCleanupError(lease.Close()) }()
	defer func() {
		retErr = errors.Join(retErr, options.Store.PruneCompletedArtifacts(completedArtifactRetention))
	}()
	createHelper := options.CreateHelper
	if createHelper == nil {
		createHelper = func(t target.Target, id string) (Helper, error) {
			helper, err := target.CreateHelper(t, id, options.TmuxRunner)
			if err != nil {
				return nil, err
			}
			return helperResource{helper: helper}, nil
		}
	}
	helper, err := createHelper(record.Target, record.ID)
	if err != nil {
		return fmt.Errorf("create Wrap tmux helper: %w", err)
	}
	defer func() { joinCleanupError(helper.Close()) }()

	newMirror := options.NewMirror
	if newMirror == nil {
		newMirror = func(helper Helper, record instance.Record) (Mirror, error) {
			hostTarget := mirror.HostSession{
				ID:         helper.ID(),
				WindowID:   record.Target.WindowID,
				Generation: record.Target.Generation,
				Name:       record.Name,
				Kind:       "terminal",
			}
			return mirror.NewManager(mirror.ManagerOptions{
				Workspace: record.Name,
				Target:    &hostTarget,
				Viewers: &mirror.PTYViewerFactory{
					Endpoint: tmux.Endpoint{SocketPath: record.Target.SocketPath},
				},
				StartServer: options.StartServer,
				StartTunnel: options.StartTunnel,
				Random:      options.Random,
				Diagnostics: options.Diagnostics,
			})
		}
	}
	mirrorService, err := newMirror(helper, record)
	if err != nil {
		return fmt.Errorf("create Wrap mirror: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		joinCleanupError(mirrorService.Shutdown(shutdownCtx))
	}()
	if err := mirrorService.Start(ctx); err != nil {
		return fmt.Errorf("start Wrap mirror: %w", err)
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	handler := &serviceHandler{
		record: record,
		store:  options.Store,
		mirror: mirrorService,
		cancel: cancelRun,
	}
	serveControl := options.ServeControl
	if serveControl == nil {
		serveControl = control.Serve
	}
	controlDone := make(chan error, 1)
	go func() {
		controlDone <- serveControl(runCtx, record.ControlSocket, record.ID, handler)
		close(controlDone)
	}()
	defer func() {
		cancelRun()
		select {
		case err := <-controlDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				joinCleanupError(err)
			}
		case <-time.After(5 * time.Second):
			joinCleanupError(errors.New("control server did not stop"))
		}
	}()
	if err := waitForControl(runCtx, record.ControlSocket, controlDone); err != nil {
		return err
	}
	if err := options.Store.Create(record); err != nil {
		return fmt.Errorf("publish Wrap instance: %w", err)
	}
	published = true
	if options.Ready != nil {
		if err := options.Ready(handler.Status()); err != nil {
			return fmt.Errorf("report Wrap readiness: %w", err)
		}
	}

	interval := options.ValidateInterval
	if interval <= 0 {
		interval = defaultValidateInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	events := mirrorService.Events()
	for {
		select {
		case <-runCtx.Done():
			if handler.shutdownRequested() {
				return nil
			}
			return runCtx.Err()
		case err := <-controlDone:
			if handler.shutdownRequested() && err == nil {
				return nil
			}
			if err == nil {
				return errors.New("wrap control server stopped")
			}
			return fmt.Errorf("wrap control server: %w", err)
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if event.Snapshot != nil && event.Snapshot.State == mirror.StateFailed {
				message := event.Snapshot.Err
				if message == "" {
					message = "mirror failed"
				}
				return errors.New(message)
			}
		case <-ticker.C:
			validationCtx, cancelValidation := context.WithTimeout(runCtx, helperValidationTimeout)
			err := helper.Validate(validationCtx)
			cancelValidation()
			if err != nil {
				return fmt.Errorf("validate Wrap target: %w", err)
			}
		}
	}
}

func waitForControl(ctx context.Context, path string, done <-chan error) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			if err == nil {
				return errors.New("wrap control server stopped before readiness")
			}
			return fmt.Errorf("start Wrap control server: %w", err)
		case <-ticker.C:
		}
	}
}

type serviceHandler struct {
	record instance.Record
	store  instance.Store
	mirror Mirror
	cancel context.CancelFunc

	mu       sync.Mutex
	shutdown bool
}

func (h *serviceHandler) Status() control.Status {
	h.mu.Lock()
	record := h.record
	h.mu.Unlock()
	snapshot := h.mirror.Snapshot()
	return control.Status{
		ID:         record.ID,
		Name:       record.Name,
		State:      mirrorStateName(snapshot.State),
		PairingURL: snapshot.PairingURL,
		QR:         snapshot.QR,
		Directory:  record.Directory,
		Clients:    h.mirror.ClientCount(),
		StartedAt:  record.StartedAt,
		Target:     record.Target,
		Warning:    snapshotWarnings(snapshot),
	}
}

func (h *serviceHandler) Rename(_ context.Context, name string) (control.Status, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	renamed, err := h.store.RenameIfPID(h.record.ID, h.record.PID, name)
	if err != nil {
		return control.Status{}, err
	}
	if !renamed {
		return control.Status{}, errors.New("wrap instance ownership changed")
	}
	h.record.Name = name
	snapshot := h.mirror.Snapshot()
	return control.Status{
		ID: h.record.ID, Name: h.record.Name, State: mirrorStateName(snapshot.State),
		PairingURL: snapshot.PairingURL, QR: snapshot.QR, Directory: h.record.Directory,
		Clients: h.mirror.ClientCount(), StartedAt: h.record.StartedAt, Target: h.record.Target,
		Warning: snapshotWarnings(snapshot),
	}, nil
}

func snapshotWarnings(snapshot mirror.Snapshot) string {
	warnings := make([]string, 0, 2)
	if snapshot.DiagnosticsWarning != "" {
		warnings = append(warnings, snapshot.DiagnosticsWarning)
	}
	if snapshot.CleanupWarning != "" {
		warnings = append(warnings, snapshot.CleanupWarning)
	}
	return strings.Join(warnings, "; ")
}

func (h *serviceHandler) Rotate(ctx context.Context) (control.Status, error) {
	if h.mirror.Snapshot().State != mirror.StateReady {
		return control.Status{}, control.ErrNotReady
	}
	if err := h.mirror.Rotate(ctx); err != nil {
		return control.Status{}, err
	}
	return h.Status(), nil
}

func (h *serviceHandler) Shutdown(context.Context) error {
	h.mu.Lock()
	h.shutdown = true
	h.mu.Unlock()
	h.cancel()
	return nil
}

func (h *serviceHandler) shutdownRequested() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.shutdown
}

func mirrorStateName(state mirror.State) string {
	switch state {
	case mirror.StateStarting:
		return "starting"
	case mirror.StateReady:
		return "ready"
	case mirror.StateFailed:
		return "failed"
	default:
		return "stopped"
	}
}
