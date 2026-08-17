package share

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sarcasticbird/wrap/internal/control"
	"github.com/sarcasticbird/wrap/internal/instance"
	"github.com/sarcasticbird/wrap/internal/target"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

const (
	MaxLaunchBytes        = 64 << 10
	maxReadyBytes         = 1 << 20
	recordFD              = 3
	readyFD               = 4
	maxStartupStderrBytes = 8 << 10
)

var defaultWorkerStartupTimeout = 45 * time.Second

const WorkerStopTimeout = 12 * time.Second

var workerStopTimeout = WorkerStopTimeout

var killWorkerProcessGroup = func(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

type readyResult struct {
	status control.Status
	err    error
}

type boundedTailWriter struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func newBoundedTailWriter(limit int) *boundedTailWriter {
	return &boundedTailWriter{limit: limit}
}

func (writer *boundedTailWriter) Write(payload []byte) (int, error) {
	written := len(payload)
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if writer.limit <= 0 {
		return written, nil
	}
	if len(payload) >= writer.limit {
		writer.data = append(writer.data[:0], payload[len(payload)-writer.limit:]...)
		return written, nil
	}
	overflow := len(writer.data) + len(payload) - writer.limit
	if overflow > 0 {
		copy(writer.data, writer.data[overflow:])
		writer.data = writer.data[:len(writer.data)-overflow]
	}
	writer.data = append(writer.data, payload...)
	return written, nil
}

func (writer *boundedTailWriter) String() string {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return string(writer.data)
}

func withStartupStderr(err error, stderr *boundedTailWriter) error {
	detail := strings.TrimSpace(stderr.String())
	if detail == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, detail)
}

func Start(ctx context.Context, executable string, record instance.Record) (control.Status, error) {
	if ctx == nil {
		return control.Status{}, errors.New("worker startup context is required")
	}
	if !filepath.IsAbs(executable) {
		return control.Status{}, errors.New("worker executable path must be absolute")
	}
	startupCtx := ctx
	cancelStartup := func() {}
	if _, ok := ctx.Deadline(); !ok {
		startupCtx, cancelStartup = context.WithTimeout(ctx, defaultWorkerStartupTimeout)
	}
	defer cancelStartup()
	validation := record
	validation.PID = 1
	if err := validation.Validate(); err != nil {
		return control.Status{}, fmt.Errorf("validate worker launch: %w", err)
	}

	recordRead, recordWrite, err := os.Pipe()
	if err != nil {
		return control.Status{}, fmt.Errorf("create worker record pipe: %w", err)
	}
	defer func() { _ = recordRead.Close() }()
	defer func() { _ = recordWrite.Close() }()
	readyRead, readyWrite, err := os.Pipe()
	if err != nil {
		return control.Status{}, fmt.Errorf("create worker readiness pipe: %w", err)
	}
	defer func() { _ = readyRead.Close() }()
	defer func() { _ = readyWrite.Close() }()

	cmd := exec.Command(executable,
		"_serve",
		"--instance", record.ID,
		"--control", record.ControlSocket,
		"--record-fd", fmt.Sprint(recordFD),
		"--ready-fd", fmt.Sprint(readyFD),
	)
	cmd.ExtraFiles = []*os.File{recordRead, readyWrite}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	startupStderr := newBoundedTailWriter(maxStartupStderrBytes)
	cmd.Stderr = startupStderr
	if err := cmd.Start(); err != nil {
		return control.Status{}, fmt.Errorf("start detached Wrap worker: %w", err)
	}
	_ = recordRead.Close()
	_ = readyWrite.Close()

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()
	record.PID = cmd.Process.Pid
	payload, err := EncodeLaunch(record)
	if err != nil {
		return control.Status{}, errors.Join(err, stopStartedProcess(cmd, waitDone, record))
	}
	if _, err := recordWrite.Write(payload); err != nil {
		return control.Status{}, errors.Join(
			fmt.Errorf("send worker launch record: %w", err),
			stopStartedProcess(cmd, waitDone, record),
		)
	}
	if err := recordWrite.Close(); err != nil {
		return control.Status{}, errors.Join(
			fmt.Errorf("close worker launch record: %w", err),
			stopStartedProcess(cmd, waitDone, record),
		)
	}

	readyDone := make(chan readyResult, 1)
	go func() {
		status, err := readReady(readyRead)
		readyDone <- readyResult{status: status, err: err}
	}()
	for {
		select {
		case <-startupCtx.Done():
			return control.Status{}, errors.Join(startupCtx.Err(), stopStartedProcess(cmd, waitDone, record))
		case result := <-readyDone:
			if result.err != nil {
				failure := errors.Join(
					fmt.Errorf("wrap worker failed before readiness: %w", result.err),
					stopStartedProcess(cmd, waitDone, record),
				)
				return control.Status{}, withStartupStderr(failure, startupStderr)
			}
			if result.status.ID != record.ID {
				failure := errors.Join(
					errors.New("worker returned mismatched instance ID"),
					stopStartedProcess(cmd, waitDone, record),
				)
				return control.Status{}, withStartupStderr(failure, startupStderr)
			}
			if waitErr, exited := completedWorkerWait(waitDone); exited {
				failure := errors.Join(
					workerEarlyExitError(waitErr),
					cleanupReapedWorkerArtifacts(record, nil),
				)
				return control.Status{}, withStartupStderr(failure, startupStderr)
			}
			return result.status, nil
		case waitErr := <-waitDone:
			failure := errors.Join(
				workerEarlyExitError(waitErr),
				cleanupReapedWorkerArtifacts(record, nil),
			)
			return control.Status{}, withStartupStderr(failure, startupStderr)
		}
	}
}

func completedWorkerWait(waitDone <-chan error) (error, bool) {
	select {
	case waitErr := <-waitDone:
		return waitErr, true
	default:
		return nil, false
	}
}

func workerEarlyExitError(waitErr error) error {
	if waitErr == nil {
		return errors.New("wrap worker exited before readiness")
	}
	return fmt.Errorf("wrap worker exited before readiness: %w", waitErr)
}

func EncodeLaunch(record instance.Record) ([]byte, error) {
	if err := record.Validate(); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := json.NewEncoder(&output).Encode(record); err != nil {
		return nil, fmt.Errorf("encode worker launch record: %w", err)
	}
	if output.Len() > MaxLaunchBytes {
		return nil, errors.New("worker launch record is too large")
	}
	return output.Bytes(), nil
}

func ReadLaunch(reader io.Reader, expectedID, expectedControlSocket string) (instance.Record, error) {
	if reader == nil {
		return instance.Record{}, errors.New("worker launch reader is required")
	}
	data, err := io.ReadAll(io.LimitReader(reader, MaxLaunchBytes+1))
	if err != nil {
		return instance.Record{}, fmt.Errorf("read worker launch record: %w", err)
	}
	if len(data) > MaxLaunchBytes {
		return instance.Record{}, errors.New("worker launch record is too large")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var record instance.Record
	if err := decoder.Decode(&record); err != nil {
		return instance.Record{}, fmt.Errorf("decode worker launch record: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return instance.Record{}, errors.New("worker launch record contains trailing JSON")
	}
	if err := record.Validate(); err != nil {
		return instance.Record{}, err
	}
	if record.ID != expectedID || record.ControlSocket != expectedControlSocket {
		return instance.Record{}, errors.New("worker launch identity does not match arguments")
	}
	return record, nil
}

func WriteReady(writer io.Writer, status control.Status) error {
	if writer == nil {
		return errors.New("worker readiness writer is required")
	}
	if status.ID == "" || status.Name == "" || status.State != "ready" || status.PairingURL == "" {
		return errors.New("worker readiness status is incomplete")
	}
	if err := json.NewEncoder(writer).Encode(status); err != nil {
		return fmt.Errorf("write worker readiness: %w", err)
	}
	return nil
}

func readReady(reader io.Reader) (control.Status, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, maxReadyBytes+1))
	decoder.DisallowUnknownFields()
	var status control.Status
	if err := decoder.Decode(&status); err != nil {
		return control.Status{}, fmt.Errorf("read worker readiness: %w", err)
	}
	if status.ID == "" || status.Name == "" || status.State != "ready" || status.PairingURL == "" {
		return control.Status{}, errors.New("worker returned incomplete readiness status")
	}
	return status, nil
}

func stopStartedProcess(cmd *exec.Cmd, waitDone <-chan error, record instance.Record) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	signalErr := cmd.Process.Signal(syscall.SIGTERM)
	if errors.Is(signalErr, os.ErrProcessDone) {
		signalErr = nil
	}
	timer := time.NewTimer(workerStopTimeout)
	defer timer.Stop()
	select {
	case waitErr := <-waitDone:
		return errors.Join(
			signalErr,
			normalizedStopWaitError(waitErr),
			cleanupReapedWorkerArtifacts(record, nil),
		)
	case <-timer.C:
	}

	groupKillErr := killWorkerProcessGroup(cmd.Process.Pid)
	if errors.Is(groupKillErr, syscall.ESRCH) {
		groupKillErr = nil
	}
	processKillErr := cmd.Process.Kill()
	if errors.Is(processKillErr, os.ErrProcessDone) {
		processKillErr = nil
	}
	var waitErr error
	workerExited := false
	select {
	case waitErr = <-waitDone:
		workerExited = true
	case <-time.After(2 * time.Second):
		waitErr = errors.New("detached Wrap worker did not exit after forced stop")
	}
	var cleanupErr error
	if workerExited {
		cleanupErr = cleanupReapedWorkerArtifacts(record, nil)
	}
	return errors.Join(
		signalErr,
		groupKillErr,
		processKillErr,
		normalizedStopWaitError(waitErr),
		cleanupErr,
	)
}

func normalizedStopWaitError(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}

func cleanupReapedWorkerArtifacts(record instance.Record, run tmux.Runner) error {
	store, err := (instance.Store{}).ForRecord(record)
	if err != nil {
		return err
	}
	lease, err := store.AcquireLease(record.ID)
	if err != nil {
		return fmt.Errorf("lock reaped Wrap worker cleanup: %w", err)
	}
	return errors.Join(cleanupStartedArtifacts(record, run), lease.Close())
}

func cleanupStartedArtifacts(record instance.Record, run tmux.Runner) error {
	helperErr := target.CleanupHelper(record.Target, record.ID, run)
	socketErr := removeStartedControlSocket(record)
	return errors.Join(helperErr, socketErr)
}

func removeStartedControlSocket(record instance.Record) error {
	path := filepath.Clean(record.ControlSocket)
	if filepath.Base(path) != record.ID+".sock" {
		return errors.New("started Wrap control socket is not derived from its instance ID")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect started Wrap control socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("started Wrap control socket path is not a socket")
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove started Wrap control socket: %w", err)
	}
	return nil
}
