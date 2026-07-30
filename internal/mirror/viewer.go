package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/creack/pty"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

type Identity struct {
	ID         string
	Generation string
}

type Viewer interface {
	Write([]byte) error
	Resize(columns, rows uint16) error
	Close() error
	Done() <-chan error
}

type ViewerFactory interface {
	Open(
		ctx context.Context,
		identity Identity,
		columns, rows uint16,
		output func([]byte) error,
	) (Viewer, error)
}

type PTYViewerFactory struct {
	SessionSocket string
	TmuxPath      string
	Environment   []string

	pinMu sync.Mutex
	pins  map[viewerWindowKey]*viewerWindowPin
}

type viewerWindowKey struct {
	generation string
	windowID   string
}

type viewerWindowPin struct {
	references        int
	originalMode      string
	originalInherited bool
}

func (f *PTYViewerFactory) Open(
	ctx context.Context,
	identity Identity,
	columns, rows uint16,
	output func([]byte) error,
) (Viewer, error) {
	if err := validateDimensions(columns, rows); err != nil {
		return nil, err
	}
	if output == nil {
		return nil, errors.New("viewer output callback is required")
	}
	windowID, releasePin, err := f.pinWindow(identity)
	if err != nil {
		return nil, err
	}
	command, err := f.buildCommand(identity, windowID)
	if err != nil {
		return nil, errors.Join(err, releasePin())
	}
	ptmx, err := pty.StartWithSize(command, &pty.Winsize{Cols: columns, Rows: rows})
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("start tmux viewer PTY: %w", err),
			releasePin(),
		)
	}
	viewer := &ptyViewer{
		command:    command,
		ptmx:       ptmx,
		output:     output,
		releasePin: releasePin,
		done:       make(chan error, 1),
		finished:   make(chan struct{}),
	}
	go viewer.run()
	go func() {
		select {
		case <-ctx.Done():
			viewer.terminate()
		case <-viewer.finished:
		}
	}()
	return viewer, nil
}

func (f *PTYViewerFactory) buildCommand(
	identity Identity,
	windowID string,
) (*exec.Cmd, error) {
	if err := validateIdentity(identity.ID, identity.Generation); err != nil {
		return nil, err
	}
	tmuxPath, socket, err := f.commandSettings()
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
	args := append([]string{"-L", socket}, attach...)
	command := exec.Command(tmuxPath, args...)
	command.Env = f.cleanEnvironment()
	return command, nil
}

func (f *PTYViewerFactory) pinWindow(
	identity Identity,
) (string, func() error, error) {
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	if f.pins == nil {
		f.pins = make(map[viewerWindowKey]*viewerWindowPin)
	}
	args, err := tmux.PinWindowSizeIfGenerationArgs(identity.ID, identity.Generation)
	if err != nil {
		return "", nil, err
	}
	result, err := f.runTmux(args)
	if err != nil {
		return "", nil, fmt.Errorf("pin mirrored tmux window size: %w", err)
	}
	windowID, mode, inherited, err := parseWindowPin(result)
	if err != nil {
		return "", nil, fmt.Errorf("pin mirrored tmux window size: %w", err)
	}
	key := viewerWindowKey{generation: identity.Generation, windowID: windowID}
	if pin := f.pins[key]; pin != nil {
		pin.references++
		return windowID, f.releaseWindowFunc(key), nil
	}
	f.pins[key] = &viewerWindowPin{
		references:        1,
		originalMode:      mode,
		originalInherited: inherited,
	}
	return windowID, f.releaseWindowFunc(key), nil
}

func parseWindowPin(result string) (windowID, mode string, inherited bool, err error) {
	lines := strings.Split(strings.TrimSpace(result), "\n")
	if len(lines) != 2 {
		return "", "", false, fmt.Errorf("unexpected tmux pin result %q", result)
	}
	windowID = strings.TrimSpace(lines[0])
	if !validWindowID(windowID) {
		return "", "", false, fmt.Errorf("unexpected tmux window id %q", windowID)
	}
	fields := strings.Fields(lines[1])
	if len(fields) != 2 {
		return "", "", false, fmt.Errorf("unexpected tmux window-size result %q", lines[1])
	}
	switch fields[0] {
	case "window-size":
	case "window-size*":
		inherited = true
	default:
		return "", "", false, fmt.Errorf("unexpected tmux window-size source %q", fields[0])
	}
	mode = fields[1]
	switch mode {
	case "largest", "smallest", "manual", "latest":
	default:
		return "", "", false, fmt.Errorf("unexpected tmux window-size mode %q", mode)
	}
	return windowID, mode, inherited, nil
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

func (f *PTYViewerFactory) releaseWindowFunc(key viewerWindowKey) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			result = f.releaseWindow(key)
		})
		return result
	}
}

func (f *PTYViewerFactory) releaseWindow(key viewerWindowKey) error {
	f.pinMu.Lock()
	defer f.pinMu.Unlock()
	pin := f.pins[key]
	if pin == nil {
		return nil
	}
	pin.references--
	if pin.references > 0 {
		return nil
	}
	delete(f.pins, key)
	args, err := tmux.RestoreWindowSizeIfGenerationArgs(
		key.generation,
		key.windowID,
		pin.originalMode,
		pin.originalInherited,
	)
	if err != nil {
		return err
	}
	if _, err := f.runTmux(args); errors.Is(err, tmux.ErrServerGenerationChanged) {
		return nil
	} else if tmux.IsMissingTargetError(err) {
		return nil
	} else if err != nil {
		return fmt.Errorf("restore mirrored tmux window size: %w", err)
	}
	return nil
}

func (f *PTYViewerFactory) runTmux(args []string) (string, error) {
	tmuxPath, socket, err := f.commandSettings()
	if err != nil {
		return "", err
	}
	commandArgs := append([]string{"-L", socket}, args...)
	command := exec.Command(tmuxPath, commandArgs...)
	command.Env = f.cleanEnvironment()
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	if strings.TrimSpace(string(output)) == "wrap-server-generation-mismatch" {
		return "", tmux.ErrServerGenerationChanged
	}
	return string(output), nil
}

func (f *PTYViewerFactory) commandSettings() (string, string, error) {
	tmuxPath := f.TmuxPath
	if tmuxPath == "" {
		var err error
		tmuxPath, err = exec.LookPath("tmux")
		if err != nil {
			return "", "", fmt.Errorf("tmux not found in PATH: %w", err)
		}
	}
	socket := f.SessionSocket
	if socket == "" {
		socket = tmux.SocketSessions
	}
	return tmuxPath, socket, nil
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
	command    *exec.Cmd
	ptmx       *os.File
	output     func([]byte) error
	releasePin func() error
	done       chan error
	finished   chan struct{}

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

func (v *ptyViewer) Resize(columns, rows uint16) error {
	if err := validateDimensions(columns, rows); err != nil {
		return err
	}
	v.ioMu.Lock()
	defer v.ioMu.Unlock()
	if err := pty.Setsize(v.ptmx, &pty.Winsize{Cols: columns, Rows: rows}); err != nil {
		return fmt.Errorf("resize viewer PTY: %w", err)
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
	waitErr = viewerWaitError(waitErr, intentional)
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
