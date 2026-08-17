package mirror

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

type Identity struct {
	ID         string
	WindowID   string
	Generation string
}

type Viewer interface {
	Write([]byte) error
	Close() error
	Done() <-chan error
}

type ViewerGeometry struct {
	Columns uint16
	Rows    uint16

	statusRows  uint16
	statusValue string
}

type viewerClientGeometry struct {
	name          string
	pid           int
	columns       uint16
	rows          uint16
	flags         string
	windowID      string
	windowColumns uint16
	windowRows    uint16
	windowBigger  bool
	statusRows    uint16
}

type PreparedViewer interface {
	Geometry() ViewerGeometry
	Start() (Viewer, error)
	Close() error
}

type ViewerFactory interface {
	Prepare(
		ctx context.Context,
		identity Identity,
		output func([]byte) error,
	) (PreparedViewer, error)
}

type PTYViewerFactory struct {
	SessionSocket string
	Endpoint      tmux.Endpoint
	TmuxPath      string
	Environment   []string
	Record        func(DiagnosticRecord)

	pinMu sync.Mutex
	pins  map[viewerWindowKey]*viewerWindowPin
	run   func([]string) (string, error)

	queryClient   func(int) (viewerClientGeometry, error)
	resizePTY     func(*os.File, ViewerGeometry) error
	refreshClient func(string) error
	waitGeometry  func(context.Context, time.Duration) error
}

const (
	viewerGeometryPollInterval = 10 * time.Millisecond
	viewerGeometryPhaseTimeout = 2 * time.Second
	viewerGeometryAttempts     = int(viewerGeometryPhaseTimeout / viewerGeometryPollInterval)
	viewerWindowPinAttempts    = 3
)

var errViewerClientNotAttached = errors.New("tmux viewer client is not attached")

type viewerWindowKey struct {
	generation string
	windowID   string
}

type viewerWindowPin struct {
	references        int
	originalMode      string
	originalInherited bool
	restore           bool
	owner             string
	hookIndex         uint32
}

func (f *PTYViewerFactory) Prepare(
	ctx context.Context,
	identity Identity,
	output func([]byte) error,
) (PreparedViewer, error) {
	if output == nil {
		return nil, errors.New("viewer output callback is required")
	}
	windowID, geometry, releasePin, err := f.pinWindow(identity)
	if err != nil {
		return nil, err
	}
	return &ptyViewerPreparation{
		factory:    f,
		ctx:        ctx,
		identity:   identity,
		windowID:   windowID,
		geometry:   geometry,
		output:     output,
		releasePin: releasePin,
	}, nil
}

type ptyViewerPreparation struct {
	factory    *PTYViewerFactory
	ctx        context.Context
	identity   Identity
	windowID   string
	geometry   ViewerGeometry
	output     func([]byte) error
	releasePin func() error

	mu      sync.Mutex
	started bool
	closed  bool
}

func (p *ptyViewerPreparation) Geometry() ViewerGeometry {
	return p.geometry
}

func (p *ptyViewerPreparation) Start() (Viewer, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("prepared viewer is closed")
	}
	if p.started {
		return nil, errors.New("prepared viewer is already started")
	}
	command, err := p.factory.buildCommand(p.identity, p.windowID)
	if err != nil {
		p.closed = true
		return nil, errors.Join(err, p.releasePin())
	}
	ptmx, err := pty.StartWithSize(command, &pty.Winsize{
		Cols: p.geometry.Columns,
		Rows: p.geometry.Rows,
	})
	if err != nil {
		p.closed = true
		return nil, errors.Join(
			fmt.Errorf("start tmux viewer PTY: %w", err),
			p.releasePin(),
		)
	}
	if command.Process == nil {
		p.closed = true
		return nil, errors.Join(
			errors.New("start tmux viewer PTY: process is unavailable"),
			stopUnverifiedViewer(command, ptmx),
			p.releasePin(),
		)
	}
	if _, err := p.factory.verifyViewerGeometry(
		p.ctx,
		command.Process.Pid,
		ptmx,
		p.windowID,
		p.geometry,
	); err != nil {
		p.closed = true
		return nil, errors.Join(
			fmt.Errorf("verify tmux viewer geometry: %w", err),
			stopUnverifiedViewer(command, ptmx),
			p.releasePin(),
		)
	}
	viewer := &ptyViewer{
		command: command,
		ptmx:    ptmx,
		output:  p.output,
		terminalEnded: func() (bool, error) {
			return p.factory.viewerTerminalEnded(p.identity)
		},
		releasePin: p.releasePin,
		done:       make(chan error, 1),
		finished:   make(chan struct{}),
	}
	go viewer.run()
	go func() {
		select {
		case <-p.ctx.Done():
			viewer.terminate()
		case <-viewer.finished:
		}
	}()
	p.started = true
	return viewer, nil
}

func stopUnverifiedViewer(command *exec.Cmd, ptmx *os.File) error {
	var result error
	if command != nil && command.Process != nil {
		if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			result = errors.Join(result, fmt.Errorf("stop unverified tmux viewer: %w", err))
		}
	}
	if ptmx != nil {
		if err := ptmx.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			result = errors.Join(result, fmt.Errorf("close unverified viewer PTY: %w", err))
		}
	}
	if command != nil && command.Process != nil {
		if err := viewerWaitError(command.Wait(), true); err != nil {
			result = errors.Join(result, fmt.Errorf("wait for unverified tmux viewer: %w", err))
		}
	}
	return result
}

func (p *ptyViewerPreparation) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started || p.closed {
		return nil
	}
	p.closed = true
	return p.releasePin()
}

func (f *PTYViewerFactory) buildCommand(
	identity Identity,
	windowID string,
) (*exec.Cmd, error) {
	if err := validateIdentity(identity.ID, identity.Generation); err != nil {
		return nil, err
	}
	tmuxPath, endpoint, err := f.commandSettings()
	if err != nil {
		return nil, err
	}
	prefix, err := endpoint.Args()
	if err != nil {
		return nil, err
	}
	attach, err := tmux.AttachWindowIgnoringSizeIfGenerationArgs(
		identity.ID,
		identity.Generation,
		windowID,
	)
	if err != nil {
		return nil, fmt.Errorf("build generation-guarded viewer attach: %w", err)
	}
	args := append(prefix, attach...)
	command := exec.Command(tmuxPath, args...)
	command.Env = f.cleanEnvironment()
	return command, nil
}

func (f *PTYViewerFactory) pinWindow(
	identity Identity,
) (string, ViewerGeometry, func() error, error) {
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	if f.pins == nil {
		f.pins = make(map[viewerWindowKey]*viewerWindowPin)
	}
	for range viewerWindowPinAttempts {
		args, err := tmux.CaptureWindowSizeIfGenerationArgs(identity.ID, identity.Generation)
		if err != nil {
			return "", ViewerGeometry{}, nil, err
		}
		result, err := f.runTmux(args)
		if err != nil {
			return "", ViewerGeometry{}, nil, fmt.Errorf("capture mirrored tmux window size: %w", err)
		}
		windowID, geometry, mode, inherited, err := parseWindowPin(result)
		if err != nil {
			return "", ViewerGeometry{}, nil, fmt.Errorf("capture mirrored tmux window size: %w", err)
		}
		if identity.WindowID != "" && windowID != identity.WindowID {
			return "", ViewerGeometry{}, nil, fmt.Errorf(
				"mirrored target window changed from %s to %s",
				identity.WindowID,
				windowID,
			)
		}
		key := viewerWindowKey{generation: identity.Generation, windowID: windowID}
		if pin := f.pins[key]; pin != nil {
			pin.references++
			return windowID, geometry, f.releaseWindowFunc(key), nil
		}
		windowRows := geometry.Rows - geometry.statusRows
		owner := ""
		var hookIndex uint32
		if inherited {
			owner, hookIndex, err = newViewerWindowPinOwner()
			if err != nil {
				return "", ViewerGeometry{}, nil, err
			}
		}
		pinArgs, err := tmux.PinWindowSizeIfGenerationArgs(
			identity.ID,
			identity.Generation,
			windowID,
			geometry.Columns,
			windowRows,
			mode,
			inherited,
			geometry.statusValue,
			owner,
			hookIndex,
		)
		if err != nil {
			return "", ViewerGeometry{}, nil, err
		}
		pinResult, err := f.runTmux(pinArgs)
		if err != nil {
			pinErr := fmt.Errorf("pin mirrored tmux window size: %w", err)
			if inherited {
				rollbackErr := f.restoreWindowPin(key, mode, inherited, owner, hookIndex)
				if rollbackErr != nil {
					return "", ViewerGeometry{}, nil, errors.Join(
						pinErr,
						fmt.Errorf("roll back partial mirrored tmux window pin: %w", rollbackErr),
					)
				}
			}
			if tmux.IsWindowPinConflictError(err) {
				continue
			}
			return "", ViewerGeometry{}, nil, pinErr
		}
		if tmux.IsWindowPinMismatchOutput(pinResult) {
			continue
		}
		if strings.TrimSpace(pinResult) != "" {
			pinErr := fmt.Errorf("unexpected tmux window pin result %q", pinResult)
			if inherited {
				if rollbackErr := f.restoreWindowPin(key, mode, inherited, owner, hookIndex); rollbackErr != nil {
					return "", ViewerGeometry{}, nil, errors.Join(
						pinErr,
						fmt.Errorf("roll back partial mirrored tmux window pin: %w", rollbackErr),
					)
				}
			}
			return "", ViewerGeometry{}, nil, pinErr
		}
		f.pins[key] = &viewerWindowPin{
			references:        1,
			originalMode:      mode,
			originalInherited: inherited,
			restore:           inherited,
			owner:             owner,
			hookIndex:         hookIndex,
		}
		return windowID, geometry, f.releaseWindowFunc(key), nil
	}
	return "", ViewerGeometry{}, nil, errors.New("tmux window state did not stabilize for mirroring")
}

func parseWindowPin(result string) (
	windowID string,
	geometry ViewerGeometry,
	mode string,
	inherited bool,
	err error,
) {
	windowID, geometry, mode, inherited, err = parseWindowPinUnchecked(result)
	if err != nil {
		return "", ViewerGeometry{}, "", false, err
	}
	if err := validateDimensions(geometry.Columns, geometry.Rows); err != nil {
		return "", ViewerGeometry{}, "", false, fmt.Errorf("invalid tmux window geometry: %w", err)
	}
	return windowID, geometry, mode, inherited, nil
}

func parseWindowPinUnchecked(result string) (
	windowID string,
	geometry ViewerGeometry,
	mode string,
	inherited bool,
	err error,
) {
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 3 {
		return "", ViewerGeometry{}, "", false, fmt.Errorf("unexpected tmux pin result %q", result)
	}
	geometryFields := strings.Split(lines[0], "\t")
	if len(geometryFields) != 3 {
		return "", ViewerGeometry{}, "", false, fmt.Errorf("unexpected tmux window geometry %q", lines[0])
	}
	windowID = strings.TrimSpace(geometryFields[0])
	if !validWindowID(windowID) {
		return "", ViewerGeometry{}, "", false, fmt.Errorf("unexpected tmux window id %q", windowID)
	}
	columns, columnsErr := strconv.ParseUint(geometryFields[1], 10, 16)
	rows, rowsErr := strconv.ParseUint(geometryFields[2], 10, 16)
	if columnsErr != nil || rowsErr != nil {
		return "", ViewerGeometry{}, "", false, fmt.Errorf("unexpected tmux window geometry %q", lines[0])
	}
	statusRows, err := parseStatusRows(lines[2])
	if err != nil {
		return "", ViewerGeometry{}, "", false, err
	}
	if rows > uint64(^uint16(0))-uint64(statusRows) {
		return "", ViewerGeometry{}, "", false, fmt.Errorf("tmux viewer rows overflow: %d + %d", rows, statusRows)
	}
	geometry = ViewerGeometry{
		Columns:     uint16(columns),
		Rows:        uint16(rows) + statusRows,
		statusRows:  statusRows,
		statusValue: strings.TrimSpace(lines[2]),
	}
	fields := strings.Fields(lines[1])
	if len(fields) != 2 {
		return "", ViewerGeometry{}, "", false, fmt.Errorf("unexpected tmux window-size result %q", lines[1])
	}
	switch fields[0] {
	case "window-size":
	case "window-size*":
		inherited = true
	default:
		return "", ViewerGeometry{}, "", false, fmt.Errorf("unexpected tmux window-size source %q", fields[0])
	}
	mode = fields[1]
	switch mode {
	case "largest", "smallest", "manual", "latest":
	default:
		return "", ViewerGeometry{}, "", false, fmt.Errorf("unexpected tmux window-size mode %q", mode)
	}
	return windowID, geometry, mode, inherited, nil
}

func parseStatusRows(value string) (uint16, error) {
	switch value = strings.TrimSpace(value); value {
	case "off":
		return 0, nil
	case "on":
		return 1, nil
	}
	rows, err := strconv.ParseUint(value, 10, 16)
	if err != nil || rows > 5 {
		return 0, fmt.Errorf("unexpected tmux status value %q", value)
	}
	return uint16(rows), nil
}

func validWindowID(windowID string) bool {
	if len(windowID) < 2 || windowID[0] != '@' {
		return false
	}
	for _, value := range windowID[1:] {
		if value < '0' || value > '9' {
			return false
		}
	}
	return true
}

func parseViewerClientGeometry(result string, targetPID int) (viewerClientGeometry, error) {
	if targetPID <= 0 {
		return viewerClientGeometry{}, fmt.Errorf("invalid viewer client pid %d", targetPID)
	}
	var match *viewerClientGeometry
	for _, line := range strings.Split(strings.TrimSpace(result), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 10 {
			return viewerClientGeometry{}, fmt.Errorf("unexpected tmux client geometry %q", line)
		}
		pidValue, err := strconv.ParseInt(fields[1], 10, 0)
		if err != nil || pidValue <= 0 {
			return viewerClientGeometry{}, fmt.Errorf("unexpected tmux client pid %q", fields[1])
		}
		columns, columnsErr := strconv.ParseUint(fields[2], 10, 16)
		rows, rowsErr := strconv.ParseUint(fields[3], 10, 16)
		windowColumns, windowColumnsErr := strconv.ParseUint(fields[6], 10, 16)
		windowRows, windowRowsErr := strconv.ParseUint(fields[7], 10, 16)
		if columnsErr != nil || rowsErr != nil || windowColumnsErr != nil || windowRowsErr != nil {
			return viewerClientGeometry{}, fmt.Errorf("unexpected tmux client dimensions %q", line)
		}
		if !validWindowID(fields[5]) {
			return viewerClientGeometry{}, fmt.Errorf("unexpected tmux client window id %q", fields[5])
		}
		var windowBigger bool
		switch fields[8] {
		case "0":
		case "1":
			windowBigger = true
		default:
			return viewerClientGeometry{}, fmt.Errorf("unexpected tmux window-bigger value %q", fields[8])
		}
		statusRows, statusErr := parseStatusRows(fields[9])
		if statusErr != nil {
			return viewerClientGeometry{}, statusErr
		}
		if int(pidValue) != targetPID {
			continue
		}
		if match != nil {
			return viewerClientGeometry{}, fmt.Errorf("duplicate tmux viewer client pid %d", targetPID)
		}
		value := viewerClientGeometry{
			name:          fields[0],
			pid:           int(pidValue),
			columns:       uint16(columns),
			rows:          uint16(rows),
			flags:         fields[4],
			windowID:      fields[5],
			windowColumns: uint16(windowColumns),
			windowRows:    uint16(windowRows),
			windowBigger:  windowBigger,
			statusRows:    statusRows,
		}
		match = &value
	}
	if match == nil {
		return viewerClientGeometry{}, fmt.Errorf("%w: pid %d", errViewerClientNotAttached, targetPID)
	}
	return *match, nil
}

func viewerGeometryMatches(
	captured ViewerGeometry,
	windowID string,
	client viewerClientGeometry,
) bool {
	return client.windowID == windowID &&
		viewerClientHasFlag(client.flags, "ignore-size") &&
		client.columns == captured.Columns &&
		client.rows == captured.Rows &&
		client.windowColumns == captured.Columns &&
		!client.windowBigger &&
		client.statusRows == captured.statusRows &&
		uint32(client.windowRows)+uint32(captured.statusRows) == uint32(captured.Rows)
}

func viewerClientHasFlag(flags, want string) bool {
	for flag := range strings.SplitSeq(flags, ",") {
		if strings.TrimSpace(flag) == want {
			return true
		}
	}
	return false
}

func (f *PTYViewerFactory) verifyViewerGeometry(
	ctx context.Context,
	pid int,
	ptmx *os.File,
	windowID string,
	captured ViewerGeometry,
) (ViewerGeometryDiagnostic, error) {
	report := viewerGeometryDiagnostic(captured, viewerClientGeometry{}, false)
	emitDiagnostic(f.Record, DiagnosticRecord{
		Level: "info", Component: "viewer", Event: "geometry_preparing",
		ViewerGeometry: &report,
	})
	corrected := false
	for attempt := 0; attempt < viewerGeometryAttempts; attempt++ {
		client, err := f.viewerClientGeometry(pid)
		if err != nil {
			if errors.Is(err, errViewerClientNotAttached) {
				if waitErr := f.waitForViewerGeometry(ctx); waitErr != nil {
					return f.failViewerGeometry(report, waitErr)
				}
				continue
			}
			return f.failViewerGeometry(report, err)
		}
		report = viewerGeometryDiagnostic(captured, client, corrected)
		if client.windowID != windowID {
			return f.failViewerGeometry(report, fmt.Errorf(
				"tmux viewer attached to window %s instead of pinned window", client.windowID,
			))
		}
		if viewerGeometryMatches(captured, windowID, client) {
			emitDiagnostic(f.Record, DiagnosticRecord{
				Level: "info", Component: "viewer", Event: "geometry_verified",
				ViewerGeometry: &report,
			})
			return report, nil
		}
		if !corrected {
			if err := f.resizeViewerPTY(ptmx, captured); err != nil {
				return f.failViewerGeometry(report, err)
			}
			if err := f.refreshViewerClient(client.name); err != nil {
				return f.failViewerGeometry(report, err)
			}
			corrected = true
			report.Corrected = true
			emitDiagnostic(f.Record, DiagnosticRecord{
				Level: "info", Component: "viewer", Event: "geometry_corrected",
				ViewerGeometry: &report,
			})
			// Attachment discovery and post-resize convergence are separate
			// bounded phases. A client first observed on the last discovery
			// attempt must still receive a verification query after correction.
			attempt = -1
		}
		if err := f.waitForViewerGeometry(ctx); err != nil {
			return f.failViewerGeometry(report, err)
		}
	}
	return f.failViewerGeometry(report, errors.New("tmux viewer geometry did not converge"))
}

func viewerGeometryDiagnostic(
	captured ViewerGeometry,
	client viewerClientGeometry,
	corrected bool,
) ViewerGeometryDiagnostic {
	return ViewerGeometryDiagnostic{
		CapturedColumns: captured.Columns,
		CapturedRows:    captured.Rows,
		ClientColumns:   client.columns,
		ClientRows:      client.rows,
		WindowColumns:   client.windowColumns,
		WindowRows:      client.windowRows,
		StatusRows:      captured.statusRows,
		Corrected:       corrected,
	}
}

func (f *PTYViewerFactory) failViewerGeometry(
	report ViewerGeometryDiagnostic,
	err error,
) (ViewerGeometryDiagnostic, error) {
	emitDiagnostic(f.Record, DiagnosticRecord{
		Level: "warn", Component: "viewer", Event: "geometry_failed",
		ViewerGeometry: &report,
	})
	return report, err
}

func (f *PTYViewerFactory) viewerClientGeometry(pid int) (viewerClientGeometry, error) {
	if f.queryClient != nil {
		return f.queryClient(pid)
	}
	result, err := f.runTmux([]string{
		"list-clients", "-F",
		"#{client_name}\t#{client_pid}\t#{client_width}\t#{client_height}\t#{client_flags}\t#{window_id}\t#{window_width}\t#{window_height}\t#{window_bigger}\t#{status}",
	})
	if err != nil {
		return viewerClientGeometry{}, fmt.Errorf("query tmux viewer client geometry: %w", err)
	}
	return parseViewerClientGeometry(result, pid)
}

func (f *PTYViewerFactory) resizeViewerPTY(ptmx *os.File, geometry ViewerGeometry) error {
	if f.resizePTY != nil {
		return f.resizePTY(ptmx, geometry)
	}
	if ptmx == nil {
		return errors.New("viewer PTY is unavailable")
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Cols: geometry.Columns, Rows: geometry.Rows}); err != nil {
		return fmt.Errorf("correct viewer PTY geometry: %w", err)
	}
	return nil
}

func (f *PTYViewerFactory) refreshViewerClient(name string) error {
	if f.refreshClient != nil {
		return f.refreshClient(name)
	}
	if name == "" {
		return errors.New("tmux viewer client name is empty")
	}
	if _, err := f.runTmux([]string{"refresh-client", "-t", name}); err != nil {
		return fmt.Errorf("refresh tmux viewer client: %w", err)
	}
	return nil
}

func (f *PTYViewerFactory) waitForViewerGeometry(ctx context.Context) error {
	if f.waitGeometry != nil {
		return f.waitGeometry(ctx, viewerGeometryPollInterval)
	}
	timer := time.NewTimer(viewerGeometryPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (f *PTYViewerFactory) releaseWindowFunc(key viewerWindowKey) func() error {
	var mu sync.Mutex
	released := false
	return func() error {
		mu.Lock()
		decrement := !released
		released = true
		defer mu.Unlock()
		return f.releaseWindow(key, decrement)
	}
}

func (f *PTYViewerFactory) releaseWindow(key viewerWindowKey, decrement bool) error {
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	pin := f.pins[key]
	if pin == nil {
		return nil
	}
	if decrement {
		pin.references--
	}
	if pin.references > 0 {
		return nil
	}
	if !pin.restore {
		delete(f.pins, key)
		return nil
	}
	if err := f.restoreWindowPin(
		key, pin.originalMode, pin.originalInherited, pin.owner, pin.hookIndex,
	); err != nil {
		return err
	}
	delete(f.pins, key)
	return nil
}

// Cleanup retries restoration for pins whose last viewer has already exited.
// Failed pins stay in the factory so a later lifecycle cleanup can try again.
func (f *PTYViewerFactory) Cleanup() error {
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	var errs []error
	for key, pin := range f.pins {
		if pin.references > 0 {
			continue
		}
		if !pin.restore {
			delete(f.pins, key)
			continue
		}
		if err := f.restoreWindowPin(
			key, pin.originalMode, pin.originalInherited, pin.owner, pin.hookIndex,
		); err != nil {
			errs = append(errs, err)
			continue
		}
		delete(f.pins, key)
	}
	return errors.Join(errs...)
}

func (f *PTYViewerFactory) restoreWindowPin(
	key viewerWindowKey,
	mode string,
	inherited bool,
	owner string,
	hookIndex uint32,
) error {
	args, err := tmux.RestoreWindowSizeIfGenerationArgs(
		key.generation,
		key.windowID,
		mode,
		inherited,
		owner,
		hookIndex,
	)
	if err != nil {
		return err
	}
	if _, err := f.runTmux(args); errors.Is(err, tmux.ErrServerGenerationChanged) {
		return nil
	} else if tmux.IsMissingTargetError(err) {
		return nil
	} else if err != nil && strings.Contains(strings.ToLower(err.Error()), "server exited unexpectedly") {
		// Killing the final session can make tmux exit between the viewer's
		// terminal-end probe and pin cleanup. There is no surviving window to
		// restore, so that shutdown race is already a successful cleanup.
		return nil
	} else if err != nil {
		return fmt.Errorf("restore mirrored tmux window size: %w", err)
	}
	return nil
}

func newViewerWindowPinOwner() (string, uint32, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", 0, fmt.Errorf("create tmux window pin owner: %w", err)
	}
	hookIndex := binary.BigEndian.Uint32(value[:4]) & 0x7fffffff
	if hookIndex == 0 {
		hookIndex = 1
	}
	return hex.EncodeToString(value[:]), hookIndex, nil
}

func (f *PTYViewerFactory) runTmux(args []string) (string, error) {
	if f.run != nil {
		return f.run(args)
	}
	tmuxPath, endpoint, err := f.commandSettings()
	if err != nil {
		return "", err
	}
	prefix, err := endpoint.Args()
	if err != nil {
		return "", err
	}
	commandArgs := append(prefix, args...)
	command := exec.Command(tmuxPath, commandArgs...)
	command.Env = f.cleanEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	if tmux.IsGenerationMismatchOutput(string(output)) {
		return "", tmux.ErrServerGenerationChanged
	}
	return string(output), nil
}

func (f *PTYViewerFactory) viewerTerminalEnded(identity Identity) (bool, error) {
	result, err := f.runTmux([]string{
		"display-message",
		"-p",
		"-t",
		identity.ID,
		"#{" + tmux.ServerGenerationOption + "}",
	})
	if errors.Is(err, tmux.ErrServerGenerationChanged) ||
		tmux.IsMissingTargetError(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("check mirrored terminal identity: %w", err)
	}
	return strings.TrimSpace(result) != identity.Generation, nil
}

func (f *PTYViewerFactory) commandSettings() (string, tmux.Endpoint, error) {
	tmuxPath := f.TmuxPath
	if tmuxPath == "" {
		var err error
		tmuxPath, err = exec.LookPath("tmux")
		if err != nil {
			return "", tmux.Endpoint{}, fmt.Errorf("tmux not found in PATH: %w", err)
		}
	}
	endpoint := f.Endpoint
	if endpoint.SocketName == "" && endpoint.SocketPath == "" {
		socket := f.SessionSocket
		if socket == "" {
			socket = tmux.SocketSessions
		}
		endpoint.SocketName = socket
	}
	if _, err := endpoint.Args(); err != nil {
		return "", tmux.Endpoint{}, err
	}
	return tmuxPath, endpoint, nil
}

func (f *PTYViewerFactory) cleanEnvironment() []string {
	environment := f.Environment
	if environment == nil {
		environment = os.Environ()
	}
	return cleanViewerEnvironment(environment)
}

func cleanViewerEnvironment(environment []string) []string {
	clean := make([]string, 0, len(environment)+1)
	for _, value := range environment {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "TMUX" || key == "TMUX_PANE" || key == "TERM" {
			continue
		}
		clean = append(clean, value)
	}
	return append(clean, "TERM=xterm-256color")
}

type ptyViewer struct {
	command       *exec.Cmd
	ptmx          *os.File
	output        func([]byte) error
	terminalEnded func() (bool, error)
	releasePin    func() error
	done          chan error
	finished      chan struct{}

	ioMu          sync.Mutex
	resultMu      sync.Mutex
	result        error
	terminateOnce sync.Once
	closing       atomic.Bool
}

func (v *ptyViewer) Write(data []byte) error {
	v.ioMu.Lock()
	defer v.ioMu.Unlock()
	if _, err := v.ptmx.Write(data); err != nil {
		return fmt.Errorf("write viewer PTY: %w", err)
	}
	return nil
}

func (v *ptyViewer) Close() error {
	v.terminate()
	<-v.finished
	v.resultMu.Lock()
	defer v.resultMu.Unlock()
	return v.result
}

func (v *ptyViewer) Done() <-chan error {
	return v.done
}

func (v *ptyViewer) terminate() {
	v.terminateOnce.Do(func() {
		v.closing.Store(true)
		_ = v.ptmx.Close()
		if v.command.Process != nil {
			_ = v.command.Process.Kill()
		}
	})
}

func (v *ptyViewer) run() {
	buffer := make([]byte, 32<<10)
	var outputErr error
	for {
		count, err := v.ptmx.Read(buffer)
		if count > 0 {
			chunk := append([]byte(nil), buffer[:count]...)
			if callbackErr := v.output(chunk); callbackErr != nil {
				outputErr = fmt.Errorf("deliver viewer output: %w", callbackErr)
				v.terminate()
				break
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) &&
				!errors.Is(err, os.ErrClosed) &&
				!errors.Is(err, syscall.EIO) {
				outputErr = fmt.Errorf("read viewer PTY: %w", err)
			}
			break
		}
	}
	waitErr := v.command.Wait()
	intentional := v.closing.Load()
	v.terminate()
	waitErr = viewerExitResult(waitErr, intentional, v.terminalEnded)
	var releaseErr error
	if v.releasePin != nil {
		releaseErr = v.releasePin()
	}
	result := errors.Join(outputErr, waitErr, releaseErr)
	v.resultMu.Lock()
	v.result = result
	v.resultMu.Unlock()
	v.done <- result
	close(v.done)
	close(v.finished)
}

func viewerWaitError(err error, intentional bool) error {
	if !intentional {
		return err
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}

func viewerExitResult(
	waitErr error,
	intentional bool,
	terminalEnded func() (bool, error),
) error {
	if waitErr == nil || intentional || terminalEnded == nil {
		return viewerWaitError(waitErr, intentional)
	}
	ended, probeErr := terminalEnded()
	return errors.Join(viewerWaitError(waitErr, ended), probeErr)
}
