package share

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sarcasticbird/wrap/internal/control"
	"github.com/sarcasticbird/wrap/internal/instance"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

func TestShareWorkerHelperProcess(t *testing.T) {
	if os.Getenv("WRAP_PROCESS_HELPER") != "1" {
		return
	}
	separator := -1
	for i, arg := range os.Args {
		if arg == "--" {
			separator = i
			break
		}
	}
	if separator < 0 || len(os.Args) < separator+2 || os.Args[separator+1] != "_serve" {
		os.Exit(90)
	}
	args := os.Args[separator+2:]
	value := func(flag string) string {
		for i := 0; i+1 < len(args); i += 2 {
			if args[i] == flag {
				return args[i+1]
			}
		}
		return ""
	}
	recordFD, _ := strconv.Atoi(value("--record-fd"))
	readyFD, _ := strconv.Atoi(value("--ready-fd"))
	recordFile := os.NewFile(uintptr(recordFD), "launch-record")
	readyFile := os.NewFile(uintptr(readyFD), "readiness")
	if recordFile == nil || readyFile == nil {
		os.Exit(91)
	}
	var gracefulSignal <-chan os.Signal
	if os.Getenv("WRAP_PROCESS_HELPER_MODE") == "graceful" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		gracefulSignal = signals
	}
	record, err := ReadLaunch(recordFile, value("--instance"), value("--control"))
	_ = recordFile.Close()
	if err != nil {
		os.Exit(92)
	}
	switch os.Getenv("WRAP_PROCESS_HELPER_MODE") {
	case "exit":
		os.Exit(7)
	case "stderr-exit":
		if err := os.MkdirAll(filepath.Dir(value("--control")), 0o700); err != nil {
			os.Exit(98)
		}
		listener, err := net.Listen("unix", value("--control"))
		if err != nil {
			os.Exit(99)
		}
		listener.(*net.UnixListener).SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			os.Exit(100)
		}
		fmt.Fprintln(os.Stderr, "create Wrap helper: captured tmux window vanished")
		os.Exit(7)
	case "hang":
		for {
			time.Sleep(time.Hour)
		}
	case "graceful":
		if err := os.WriteFile(os.Getenv("WRAP_PROCESS_HELPER_ARMED"), []byte("armed"), 0o600); err != nil {
			os.Exit(95)
		}
		<-gracefulSignal
		if err := os.WriteFile(os.Getenv("WRAP_PROCESS_HELPER_MARKER"), []byte("cleaned"), 0o600); err != nil {
			os.Exit(96)
		}
		os.Exit(0)
	case "ignore-term":
		signal.Ignore(syscall.SIGTERM)
		if err := os.WriteFile(os.Getenv("WRAP_PROCESS_HELPER_ARMED"), []byte("armed"), 0o600); err != nil {
			os.Exit(97)
		}
		for {
			time.Sleep(time.Hour)
		}
	}
	if record.PID != os.Getpid() {
		os.Exit(93)
	}
	err = WriteReady(readyFile, control.Status{
		ID:         record.ID,
		Name:       record.Name,
		State:      "ready",
		PairingURL: "https://quiet-river.trycloudflare.com/#k=secret",
		Target:     record.Target,
	})
	_ = readyFile.Close()
	if err != nil {
		os.Exit(94)
	}
	time.Sleep(50 * time.Millisecond)
	os.Exit(0)
}

func helperExecutable(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wrap-process-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "worker")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=TestShareWorkerHelperProcess -- \"$@\"\n", os.Args[0])
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStartDetachedWorkerReturnsMatchingReadiness(t *testing.T) {
	t.Setenv("WRAP_PROCESS_HELPER", "1")
	t.Setenv("WRAP_PROCESS_HELPER_MODE", "ready")
	record, _ := shareTestRecord(t)
	record.PID = 1
	status, err := Start(t.Context(), helperExecutable(t), record)
	if err != nil {
		t.Fatal(err)
	}
	if status.ID != record.ID || status.Name != record.Name || status.PairingURL == "" {
		t.Fatalf("Start() = %#v", status)
	}
}

func TestStartDetachedWorkerReportsEarlyExit(t *testing.T) {
	t.Setenv("WRAP_PROCESS_HELPER", "1")
	t.Setenv("WRAP_PROCESS_HELPER_MODE", "exit")
	record, _ := shareTestRecord(t)
	_, err := Start(t.Context(), helperExecutable(t), record)
	if err == nil || !strings.Contains(err.Error(), "before readiness") {
		t.Fatalf("Start() = %v", err)
	}
}

func TestCompletedWorkerWaitRejectsBufferedExit(t *testing.T) {
	waitDone := make(chan error, 1)
	waitDone <- errors.New("worker failed")
	waitErr, exited := completedWorkerWait(waitDone)
	if !exited || waitErr == nil || waitErr.Error() != "worker failed" {
		t.Fatalf("completedWorkerWait() = %v, %t", waitErr, exited)
	}
}

func TestStartDetachedWorkerIncludesBoundedStartupStderr(t *testing.T) {
	t.Setenv("WRAP_PROCESS_HELPER", "1")
	t.Setenv("WRAP_PROCESS_HELPER_MODE", "stderr-exit")
	setMissingTmux(t)
	record, _ := shareTestRecord(t)
	_, err := Start(t.Context(), helperExecutable(t), record)
	if err == nil || !strings.Contains(err.Error(), "captured tmux window vanished") {
		t.Fatalf("Start() = %v", err)
	}
	if _, statErr := os.Lstat(record.ControlSocket); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("control socket after failed startup = %v", statErr)
	}
}

func setMissingTmux(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho \"can't find session\" >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestBoundedTailWriterRetainsOnlyTail(t *testing.T) {
	writer := newBoundedTailWriter(5)
	if written, err := writer.Write([]byte("abc")); err != nil || written != 3 {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	if written, err := writer.Write([]byte("defgh")); err != nil || written != 5 {
		t.Fatalf("Write() = %d, %v", written, err)
	}
	if got := writer.String(); got != "defgh" {
		t.Fatalf("String() = %q, want %q", got, "defgh")
	}
}

func TestStartDetachedWorkerHonorsReadinessTimeout(t *testing.T) {
	t.Setenv("WRAP_PROCESS_HELPER", "1")
	t.Setenv("WRAP_PROCESS_HELPER_MODE", "hang")
	record, _ := shareTestRecord(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := Start(ctx, helperExecutable(t), record)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start() = %v, want deadline exceeded", err)
	}
}

func TestStartDetachedWorkerHasDefaultReadinessTimeout(t *testing.T) {
	t.Setenv("WRAP_PROCESS_HELPER", "1")
	t.Setenv("WRAP_PROCESS_HELPER_MODE", "hang")
	previous := defaultWorkerStartupTimeout
	defaultWorkerStartupTimeout = 30 * time.Millisecond
	t.Cleanup(func() { defaultWorkerStartupTimeout = previous })
	record, _ := shareTestRecord(t)
	_, err := Start(context.Background(), helperExecutable(t), record)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Start() = %v, want default deadline exceeded", err)
	}
}

func TestStartCancellationAllowsWorkerGracefulCleanup(t *testing.T) {
	t.Setenv("WRAP_PROCESS_HELPER", "1")
	t.Setenv("WRAP_PROCESS_HELPER_MODE", "graceful")
	marker := filepath.Join(t.TempDir(), "cleanup-complete")
	armed := filepath.Join(t.TempDir(), "worker-armed")
	t.Setenv("WRAP_PROCESS_HELPER_MARKER", marker)
	t.Setenv("WRAP_PROCESS_HELPER_ARMED", armed)
	previous := workerStopTimeout
	workerStopTimeout = time.Second
	t.Cleanup(func() { workerStopTimeout = previous })
	record, _ := shareTestRecord(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cancelResult := cancelWhenFileExists(ctx, armed, cancel)
	_, err := Start(ctx, helperExecutable(t), record)
	if cancelErr := <-cancelResult; cancelErr != nil {
		t.Fatal(cancelErr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() = %v, want canceled", err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "cleaned" {
		t.Fatalf("graceful worker cleanup = %q, %v", data, err)
	}
}

func TestStartCancellationKillsWorkerProcessGroupAfterGracePeriod(t *testing.T) {
	t.Setenv("WRAP_PROCESS_HELPER", "1")
	t.Setenv("WRAP_PROCESS_HELPER_MODE", "ignore-term")
	armed := filepath.Join(t.TempDir(), "worker-armed")
	t.Setenv("WRAP_PROCESS_HELPER_ARMED", armed)
	previous := workerStopTimeout
	workerStopTimeout = 50 * time.Millisecond
	t.Cleanup(func() { workerStopTimeout = previous })
	previousKill := killWorkerProcessGroup
	var killedGroupPID int
	killWorkerProcessGroup = func(pid int) error {
		killedGroupPID = pid
		return syscall.Kill(-pid, syscall.SIGKILL)
	}
	t.Cleanup(func() { killWorkerProcessGroup = previousKill })
	record, _ := shareTestRecord(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cancelResult := cancelWhenFileExists(ctx, armed, cancel)
	_, err := Start(ctx, helperExecutable(t), record)
	if cancelErr := <-cancelResult; cancelErr != nil {
		t.Fatal(cancelErr)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() = %v, want canceled", err)
	}
	if killedGroupPID <= 0 {
		t.Fatalf("worker process group kill PID = %d", killedGroupPID)
	}
}

func cancelWhenFileExists(ctx context.Context, path string, cancel context.CancelFunc) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			if _, err := os.Stat(path); err == nil {
				cancel()
				done <- nil
				return
			} else if !errors.Is(err, os.ErrNotExist) {
				done <- err
				return
			}
			select {
			case <-ctx.Done():
				done <- ctx.Err()
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

type processRunner struct {
	calls int
	err   error
}

func (r *processRunner) Run(...string) (string, error) {
	r.calls++
	return "", r.err
}

func TestCleanupStartedArtifactsRemovesOwnedSocket(t *testing.T) {
	record, _ := shareTestRecord(t)
	if err := os.MkdirAll(filepath.Dir(record.ControlSocket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", record.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	runner := &processRunner{err: tmux.ErrMissingTarget}
	if err := cleanupStartedArtifacts(record, runner); err != nil {
		t.Fatal(err)
	}
	if runner.calls != 1 {
		t.Fatalf("helper cleanup calls = %d, want 1", runner.calls)
	}
	if _, err := os.Lstat(record.ControlSocket); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("control socket after cleanup = %v", err)
	}
}

func TestCleanupReapedWorkerArtifactsRequiresUnownedLease(t *testing.T) {
	record, store := shareTestRecord(t)
	if err := os.MkdirAll(filepath.Dir(record.ControlSocket), 0o700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", record.ControlSocket)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireLease(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	runner := &processRunner{err: tmux.ErrMissingTarget}
	err = cleanupReapedWorkerArtifacts(record, runner)
	if !errors.Is(err, instance.ErrLeaseHeld) {
		t.Fatalf("cleanupReapedWorkerArtifacts() = %v, want held lease", err)
	}
	if runner.calls != 0 {
		t.Fatalf("helper cleanup calls = %d, want none", runner.calls)
	}
	if _, statErr := os.Lstat(record.ControlSocket); statErr != nil {
		t.Fatalf("owned worker socket changed: %v", statErr)
	}
}

func TestReadLaunchValidatesExpectedIdentityAndBounds(t *testing.T) {
	record, _ := shareTestRecord(t)
	record.PID = os.Getpid()
	data, err := EncodeLaunch(record)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLaunch(strings.NewReader(string(data)), "wrong-id", record.ControlSocket); err == nil {
		t.Fatal("ReadLaunch accepted wrong instance ID")
	}
	oversized := strings.NewReader(strings.Repeat("x", MaxLaunchBytes+1))
	if _, err := ReadLaunch(oversized, record.ID, record.ControlSocket); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("ReadLaunch oversized = %v", err)
	}
}

func TestWriteReadyRejectsIncompleteStatus(t *testing.T) {
	var output strings.Builder
	err := WriteReady(&output, control.Status{ID: "01KWRAPSHARE", State: "starting"})
	if err == nil {
		t.Fatal("WriteReady accepted non-ready status")
	}
}
