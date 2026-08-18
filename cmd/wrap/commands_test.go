package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sarcasticbird/wrap/internal/control"
	"github.com/sarcasticbird/wrap/internal/instance"
	"github.com/sarcasticbird/wrap/internal/share"
	"github.com/sarcasticbird/wrap/internal/target"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

func testApplication(t *testing.T) (*application, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	var stdout, stderr bytes.Buffer
	app := &application{
		out: &stdout,
		err: &stderr,
		store: instance.Store{
			StateRoot: filepath.Join(root, "state"), RuntimeRoot: filepath.Join(root, "runtime"),
		},
		getenv:       func(string) string { return "" },
		getwd:        func() (string, error) { return root, nil },
		evalSymlinks: filepath.EvalSymlinks,
		executable:   func() (string, error) { return "/opt/bin/wrap", nil },
		lookPath:     func(name string) (string, error) { return "/opt/bin/" + name, nil },
		runCommand: func(_ context.Context, _ string, args ...string) (string, error) {
			if len(args) == 1 && args[0] == "-V" {
				return "tmux 3.5a", nil
			}
			return "cloudflared version 2026.7.0", nil
		},
		now:            func() time.Time { return time.Unix(100, 0).UTC() },
		resolveTarget:  target.ResolveCurrent,
		resolveShell:   func(func(string) string, tmux.Runner) (string, error) { return "/bin/sh", nil },
		resolveCommand: func(func(string) string, tmux.Runner) (string, error) { return "", nil },
		createSession:  target.CreateDefaultSession,
		startWorker:    nil,
		callControl:    control.Call,
		cleanupStale:   func(instance.Record) error { return nil },
		startContext: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		random:         bytes.NewReader(bytes.Repeat([]byte{0x42}, 32)),
		managementWait: time.Second,
	}
	return app, &stdout, &stderr
}

func commandTestTarget() target.Target {
	return target.Target{
		SocketPath:  "/tmp/tmux.sock",
		Generation:  "0123456789abcdef0123456789abcdef",
		SessionID:   "$1",
		WindowID:    "@2",
		SessionName: "dev",
		WindowName:  "shell",
		Directory:   "/work/api",
	}
}

func commandTestRecord(t *testing.T, app *application) instance.Record {
	t.Helper()
	record := instance.Record{
		Version:       instance.RecordVersion,
		ID:            "01KWRAPCOMMAND",
		Name:          "api",
		PID:           os.Getpid(),
		ControlSocket: filepath.Join(app.store.RuntimeRoot, "01KWRAPCOMMAND.sock"),
		StartedAt:     time.Unix(100, 0).UTC(),
		Directory:     "/work/api",
		Target:        commandTestTarget(),
	}
	if err := app.store.Create(record); err != nil {
		t.Fatal(err)
	}
	return record
}

func commandTestStatus(record instance.Record) control.Status {
	return control.Status{
		ID: record.ID, Name: record.Name, State: "ready",
		PairingURL: "https://quiet-river.trycloudflare.com/#k=secret",
		QR:         "qr\n", Directory: record.Directory, StartedAt: record.StartedAt,
		Target: record.Target,
	}
}

type readyWriteCloser struct {
	bytes.Buffer
	closed      bool
	beforeWrite func() error
}

func (writer *readyWriteCloser) Write(payload []byte) (int, error) {
	if writer.beforeWrite != nil {
		if err := writer.beforeWrite(); err != nil {
			return 0, err
		}
	}
	return writer.Buffer.Write(payload)
}

func (writer *readyWriteCloser) Close() error {
	writer.closed = true
	return nil
}

func TestReportWorkerReadyDoesNotPublishBeforeStderrRedirect(t *testing.T) {
	writer := &readyWriteCloser{}
	redirectErr := errors.New("redirect stderr")
	status := control.Status{
		ID: "01KWRAPREADY", Name: "ready", State: "ready",
		PairingURL: "https://quiet-river.trycloudflare.com/#k=secret",
	}
	err := reportWorkerReady(writer, status, func() error { return redirectErr })
	if !errors.Is(err, redirectErr) {
		t.Fatalf("reportWorkerReady() = %v, want redirect error", err)
	}
	if writer.Len() != 0 || writer.closed {
		t.Fatalf("readiness published before stderr redirect: bytes=%d closed=%t", writer.Len(), writer.closed)
	}
}

func TestReportWorkerReadyPublishesAfterStderrRedirect(t *testing.T) {
	redirected := false
	writer := &readyWriteCloser{beforeWrite: func() error {
		if !redirected {
			return errors.New("readiness written before stderr redirect")
		}
		return nil
	}}
	status := control.Status{
		ID: "01KWRAPREADY", Name: "ready", State: "ready",
		PairingURL: "https://quiet-river.trycloudflare.com/#k=secret",
	}
	if err := reportWorkerReady(writer, status, func() error {
		redirected = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !writer.closed {
		t.Fatal("readiness writer was not closed")
	}
	var got control.Status
	if err := json.Unmarshal(writer.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != status.ID || got.PairingURL != status.PairingURL {
		t.Fatalf("readiness status = %#v, want %#v", got, status)
	}
}

func TestRedirectWorkerStderrPreventsBrokenPipe(t *testing.T) {
	if os.Getenv("WRAP_STDERR_HELPER") == "1" {
		if err := redirectWorkerStderr(); err != nil {
			os.Exit(90)
		}
		if _, err := fmt.Fprintln(os.Stderr, "post-readiness failure"); err != nil {
			os.Exit(91)
		}
		return
	}
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=TestRedirectWorkerStderrPreventsBrokenPipe")
	command.Env = append(os.Environ(), "WRAP_STDERR_HELPER=1")
	command.Stderr = writeEnd
	if err := command.Start(); err != nil {
		_ = readEnd.Close()
		_ = writeEnd.Close()
		t.Fatal(err)
	}
	_ = writeEnd.Close()
	_ = readEnd.Close()
	if err := command.Wait(); err != nil {
		t.Fatalf("worker with detached stderr exited abnormally: %v", err)
	}
}

func TestStartOutsideTmuxCreatesOrdinarySessionInPhysicalDirectory(t *testing.T) {
	app, _, _ := testApplication(t)
	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	app.getwd = func() (string, error) { return link, nil }
	var gotDir, gotCommand string
	app.createSession = func(dir, command string, runner tmux.Runner) error {
		if runner != nil {
			t.Fatal("outside start supplied a private tmux runner")
		}
		gotDir, gotCommand = dir, command
		return nil
	}
	if err := app.start("api team"); err != nil {
		t.Fatal(err)
	}
	wantDir, err := filepath.EvalSymlinks(realDir)
	if err != nil {
		t.Fatal(err)
	}
	if gotDir != wantDir {
		t.Fatalf("session directory = %q, want %q", gotDir, wantDir)
	}
	if gotCommand != "'/opt/bin/wrap' _bootstrap --name 'api team'" {
		t.Fatalf("bootstrap command = %q", gotCommand)
	}
	if strings.Contains(gotCommand, "-L wrap") {
		t.Fatalf("bootstrap uses legacy private socket: %q", gotCommand)
	}
}

func TestStartInsideTmuxStartsCurrentWindowAndPrintsPairing(t *testing.T) {
	app, stdout, _ := testApplication(t)
	app.getenv = func(name string) string {
		if name == "TMUX" {
			return "/tmp/tmux.sock,123,0"
		}
		return ""
	}
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) {
		return commandTestTarget(), nil
	}
	var launched instance.Record
	app.startWorker = func(_ context.Context, executable string, record instance.Record) (control.Status, error) {
		launched = record
		status := commandTestStatus(record)
		return status, nil
	}
	if err := app.start(""); err != nil {
		t.Fatal(err)
	}
	if launched.Name != "api" || launched.Target.WindowID != "@2" || launched.ControlSocket == "" {
		t.Fatalf("worker record = %+v", launched)
	}
	if !strings.Contains(stdout.String(), launched.ID) || !strings.Contains(stdout.String(), "#k=secret") {
		t.Fatalf("start output = %q", stdout.String())
	}
}

func TestStartInsideTmuxPropagatesStartupCancellation(t *testing.T) {
	app, _, _ := testApplication(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopCalled := false
	app.startContext = func() (context.Context, context.CancelFunc) {
		return ctx, func() { stopCalled = true }
	}
	app.getenv = func(name string) string {
		if name == "TMUX" {
			return "/tmp/tmux.sock,123,0"
		}
		return ""
	}
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) {
		return commandTestTarget(), nil
	}
	app.startWorker = func(ctx context.Context, _ string, _ instance.Record) (control.Status, error) {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("worker startup context error = %v, want canceled", ctx.Err())
		}
		return control.Status{}, ctx.Err()
	}
	if err := app.start(""); !errors.Is(err, context.Canceled) {
		t.Fatalf("start() = %v, want context cancellation", err)
	}
	if !stopCalled {
		t.Fatal("start did not stop signal handling")
	}
}

func TestStartInsideTmuxReturnsExistingWrap(t *testing.T) {
	app, stdout, _ := testApplication(t)
	record := commandTestRecord(t, app)
	app.getenv = func(string) string { return "/tmp/tmux.sock,123,0" }
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) { return record.Target, nil }
	app.startWorker = func(context.Context, string, instance.Record) (control.Status, error) {
		t.Fatal("existing target started a second worker")
		return control.Status{}, nil
	}
	app.lookPath = func(name string) (string, error) {
		if name == "cloudflared" {
			return "", errors.New("missing")
		}
		return "/opt/bin/" + name, nil
	}
	app.callControl = func(_ context.Context, _ string, request control.Request) (control.Status, error) {
		if request.Action != control.ActionStatus || request.InstanceID != record.ID {
			t.Fatalf("status request = %+v", request)
		}
		return commandTestStatus(record), nil
	}
	if err := app.start(""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), record.ID) {
		t.Fatalf("existing output = %q", stdout.String())
	}
}

func TestStartInsideTmuxRenamesExistingWrapWithoutRestart(t *testing.T) {
	app, stdout, _ := testApplication(t)
	record := commandTestRecord(t, app)
	app.getenv = func(string) string { return "/tmp/tmux.sock,123,0" }
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) { return record.Target, nil }
	app.startWorker = func(context.Context, string, instance.Record) (control.Status, error) {
		t.Fatal("rename started a second worker")
		return control.Status{}, nil
	}
	var actions []string
	app.callControl = func(_ context.Context, _ string, request control.Request) (control.Status, error) {
		actions = append(actions, request.Action)
		status := commandTestStatus(record)
		if request.Action == control.ActionRename {
			status.Name = request.Name
		}
		return status, nil
	}
	if err := app.start("api two"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(actions, ",") != "status,rename" || !strings.Contains(stdout.String(), "api two") {
		t.Fatalf("rename actions/output = %v / %q", actions, stdout.String())
	}
}

func TestStartInsideTmuxReconcilesDeadRecordBeforeRestart(t *testing.T) {
	app, _, _ := testApplication(t)
	stale := commandTestRecord(t, app)
	app.getenv = func(string) string { return "/tmp/tmux.sock,123,0" }
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) { return stale.Target, nil }
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		return control.Status{}, errors.New("connection refused")
	}
	app.startWorker = func(_ context.Context, _ string, record instance.Record) (control.Status, error) {
		if record.Name != stale.Name || record.ID == stale.ID {
			t.Fatalf("replacement record = %+v", record)
		}
		return commandTestStatus(record), nil
	}
	if err := app.start(stale.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.Resolve(stale.ID); !errors.Is(err, instance.ErrNotFound) {
		t.Fatalf("stale record still resolves: %v", err)
	}
}

func TestStartInsideTmuxRetainsDeadRecordWhenCleanupFails(t *testing.T) {
	app, _, _ := testApplication(t)
	stale := commandTestRecord(t, app)
	app.getenv = func(string) string { return "/tmp/tmux.sock,123,0" }
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) { return stale.Target, nil }
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		return control.Status{}, errors.New("connection refused")
	}
	app.cleanupStale = func(instance.Record) error { return errors.New("tmux temporarily unavailable") }
	app.startWorker = func(context.Context, string, instance.Record) (control.Status, error) {
		t.Fatal("replacement worker started before stale cleanup completed")
		return control.Status{}, nil
	}
	err := app.start(stale.Name)
	if err == nil || !strings.Contains(err.Error(), "tmux temporarily unavailable") {
		t.Fatalf("start with failed stale cleanup = %v", err)
	}
	if stored, err := app.store.Resolve(stale.ID); err != nil || stored.ID != stale.ID {
		t.Fatalf("stale record after failed cleanup = %+v, %v", stored, err)
	}
}

func TestStartInsideTmuxPreservesUnreachableRecordWithHeldWorkerLease(t *testing.T) {
	app, _, _ := testApplication(t)
	record := commandTestRecord(t, app)
	lease, err := app.store.AcquireLease(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	app.getenv = func(string) string { return "/tmp/tmux.sock,123,0" }
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) { return record.Target, nil }
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		return control.Status{}, errors.New("temporarily unreachable")
	}
	app.startWorker = func(context.Context, string, instance.Record) (control.Status, error) {
		t.Fatal("held worker lease was replaced")
		return control.Status{}, nil
	}
	err = app.start("")
	if err == nil || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("start with held lease = %v", err)
	}
	if _, err := app.store.Resolve(record.ID); err != nil {
		t.Fatalf("held worker record was removed: %v", err)
	}
}

func TestReconcileUsesRuntimeLeaseRecordedByWorker(t *testing.T) {
	app, _, _ := testApplication(t)
	record := commandTestRecord(t, app)
	if err := app.store.Remove(record.ID); err != nil {
		t.Fatal(err)
	}
	recordedRuntime := filepath.Join(t.TempDir(), "old-runtime")
	record.ControlSocket = filepath.Join(recordedRuntime, record.ID+".sock")
	if err := app.store.Create(record); err != nil {
		t.Fatal(err)
	}
	recordStore, err := app.store.ForRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := recordStore.AcquireLease(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		return control.Status{}, errors.New("temporarily unreachable")
	}
	inspected, _, err := app.inspectRecords(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected) != 1 || inspected[0].record.ID != record.ID {
		t.Fatalf("inspected records = %+v", inspected)
	}
	if _, err := app.store.Resolve(record.ID); err != nil {
		t.Fatalf("record with held original lease was removed: %v", err)
	}
}

func TestListOmitsPairingMaterialButShowAndRegenReturnIt(t *testing.T) {
	app, stdout, _ := testApplication(t)
	record := commandTestRecord(t, app)
	var actions []string
	app.callControl = func(_ context.Context, _ string, request control.Request) (control.Status, error) {
		actions = append(actions, request.Action)
		status := commandTestStatus(record)
		if request.Action == control.ActionRotate {
			status.PairingURL = "https://quiet-river.trycloudflare.com/#k=rotated"
		}
		return status, nil
	}
	if err := app.list(true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), "pairing") || strings.Contains(stdout.String(), "#k=") || strings.Contains(stdout.String(), "secret") {
		t.Fatalf("list leaked pairing material: %s", stdout.String())
	}
	stdout.Reset()
	if err := app.show("api", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "#k=secret") {
		t.Fatalf("show output = %s", stdout.String())
	}
	stdout.Reset()
	if err := app.regen(record.ID, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "#k=rotated") {
		t.Fatalf("regen output = %s", stdout.String())
	}
	if strings.Join(actions, ",") != "status,status,rotate" {
		t.Fatalf("control actions = %v", actions)
	}
}

func TestWriteStatusSurfacesRotationCleanupWarning(t *testing.T) {
	var output bytes.Buffer
	status := control.Status{
		ID: "01KWRAPCOMMAND", Name: "api", PairingURL: "https://example.test/#k=secret",
		Warning: "pairing rotated, but terminal cleanup was incomplete",
	}
	if err := writeStatus(&output, status, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Warning: pairing rotated, but terminal cleanup was incomplete") {
		t.Fatalf("status output = %q", output.String())
	}
}

func TestHumanListIncludesTargetAndStartTime(t *testing.T) {
	app, stdout, _ := testApplication(t)
	record := commandTestRecord(t, app)
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		return commandTestStatus(record), nil
	}
	if err := app.list(false); err != nil {
		t.Fatal(err)
	}
	for _, wanted := range []string{record.Target.SocketPath, record.Target.WindowID, record.StartedAt.Format(time.RFC3339)} {
		if !strings.Contains(stdout.String(), wanted) {
			t.Fatalf("list output %q does not contain %q", stdout.String(), wanted)
		}
	}
}

func TestHumanListRendersAlignedUnquotedTable(t *testing.T) {
	app, stdout, _ := testApplication(t)
	record := commandTestRecord(t, app)
	status := commandTestStatus(record)
	status.Name = "api team"
	status.Directory = "/work/api team"
	status.StartedAt = time.Date(2026, 8, 18, 12, 34, 20, 0, time.UTC)
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		return status, nil
	}
	if err := app.list(false); err != nil {
		t.Fatal(err)
	}
	output := stdout.String()
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), ",") != "NAME,ID,STATE,CLIENTS,DIRECTORY,TARGET,STARTED" {
		t.Fatalf("list table header/rows = %q", output)
	}
	if strings.Contains(output, "\t") {
		t.Fatalf("list table retained raw tab separators: %q", output)
	}
	for _, unwanted := range []string{`"api team"`, `"/work/api team"`, `"/tmp/tmux.sock"`, `"@2"`} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("list table contains cosmetic quotes %q: %q", unwanted, output)
		}
	}
	for _, wanted := range []string{
		"api team", "01KWRAPCOM", "ready", "/work/api team",
		"/tmp/tmux.sock @2", "2026-08-18T12:34:20Z",
	} {
		if !strings.Contains(output, wanted) {
			t.Fatalf("list table %q does not contain %q", output, wanted)
		}
	}
}

func TestHumanListEscapesTerminalControlBytes(t *testing.T) {
	app, stdout, _ := testApplication(t)
	record := commandTestRecord(t, app)
	status := commandTestStatus(record)
	status.Directory = "/work/\x1b]52;c;Y2xpcGJvYXJk\a"
	status.Target.SocketPath = "/tmp/\x1b[31mowned"
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		return status, nil
	}
	if err := app.list(false); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(stdout.String(), "\x1b\a") {
		t.Fatalf("human list emitted terminal controls: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), `\x1b`) || !strings.Contains(stdout.String(), `\a`) {
		t.Fatalf("human list did not visibly escape controls: %q", stdout.String())
	}
}

func TestHumanWarningsAndDoctorEscapeTerminalControlBytes(t *testing.T) {
	app, stdout, stderr := testApplication(t)
	_ = commandTestRecord(t, app)
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		return control.Status{}, errors.New("dial /tmp/\x1b]52;c;Y2xpcA\a")
	}
	lease, err := app.store.AcquireLease("01KWRAPCOMMAND")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if err := app.list(false); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(stderr.String(), "\x1b\a") || !strings.Contains(stderr.String(), `\x1b`) {
		t.Fatalf("warning output = %q", stderr.String())
	}
	stderr.Reset()
	stdout.Reset()
	app.lookPath = func(name string) (string, error) { return "/tmp/\x1b[31m/" + name, nil }
	app.runCommand = func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) == 1 && args[0] == "-V" {
			return "tmux 3.5a", nil
		}
		if len(args) == 1 && args[0] == "--version" {
			return "cloudflared version 2026.7.0", nil
		}
		return "", errors.New("no legacy server")
	}
	if err := app.doctor(false); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(stdout.String(), '\x1b') || !strings.Contains(stdout.String(), `\x1b`) {
		t.Fatalf("doctor output = %q", stdout.String())
	}
}

func TestHumanDoctorSurfacesStateInspectionFailureAndReturnsError(t *testing.T) {
	app, stdout, _ := testApplication(t)
	if err := os.MkdirAll(app.store.StateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(app.store.InstancesDir(), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := app.doctor(false)
	if err == nil || !strings.Contains(err.Error(), "state inspection incomplete") {
		t.Fatalf("doctor state error = %v", err)
	}
	if !strings.Contains(stdout.String(), "state: instance path is not a regular directory") {
		t.Fatalf("doctor output = %q", stdout.String())
	}
}

func TestRemoveStopsOnlyWorkerAndWaitsForRecordCleanup(t *testing.T) {
	app, stdout, _ := testApplication(t)
	record := commandTestRecord(t, app)
	app.callControl = func(_ context.Context, _ string, request control.Request) (control.Status, error) {
		if request.Action != control.ActionShutdown {
			t.Fatalf("remove action = %q", request.Action)
		}
		if err := app.store.Remove(record.ID); err != nil {
			t.Fatal(err)
		}
		return commandTestStatus(record), nil
	}
	if err := app.remove("api"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "tmux window is still running") {
		t.Fatalf("remove output = %q", stdout.String())
	}
}

func TestRemoveReconcilesProvenStaleRecord(t *testing.T) {
	app, stdout, _ := testApplication(t)
	record := commandTestRecord(t, app)
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		return control.Status{}, errors.New("connection refused")
	}
	if err := app.remove(record.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.Resolve(record.ID); !errors.Is(err, instance.ErrNotFound) {
		t.Fatalf("stale record remains: %v", err)
	}
	if !strings.Contains(stdout.String(), "stale Wrap") || !strings.Contains(stdout.String(), "tmux window is still running") {
		t.Fatalf("remove stale output = %q", stdout.String())
	}
}

func TestRemoveRetainsProvenStaleRecordWhenCleanupFails(t *testing.T) {
	app, _, _ := testApplication(t)
	record := commandTestRecord(t, app)
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		return control.Status{}, errors.New("connection refused")
	}
	app.cleanupStale = func(instance.Record) error { return errors.New("tmux temporarily unavailable") }
	err := app.remove(record.Name)
	if err == nil || !strings.Contains(err.Error(), "tmux temporarily unavailable") {
		t.Fatalf("remove with failed stale cleanup = %v", err)
	}
	if stored, err := app.store.Resolve(record.ID); err != nil || stored.ID != record.ID {
		t.Fatalf("stale record after failed cleanup = %+v, %v", stored, err)
	}
}

func TestRemoveWaitsForWorkerLeaseAfterRecordDisappears(t *testing.T) {
	app, _, _ := testApplication(t)
	record := commandTestRecord(t, app)
	lease, err := app.store.AcquireLease(record.ID)
	if err != nil {
		t.Fatal(err)
	}
	shutdownCalled := make(chan struct{})
	app.callControl = func(context.Context, string, control.Request) (control.Status, error) {
		close(shutdownCalled)
		return commandTestStatus(record), nil
	}
	done := make(chan error, 1)
	go func() { done <- app.remove(record.Name) }()
	<-shutdownCalled
	removed, err := app.store.RemoveIfPID(record.ID, record.PID)
	if err != nil || !removed {
		t.Fatalf("remove record fixture = %v, %v", removed, err)
	}
	select {
	case err := <-done:
		t.Fatalf("remove returned before lease release: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("remove did not finish after lease release")
	}
}

func TestDefaultManagementWaitExceedsWorkerStopTimeout(t *testing.T) {
	if defaultManagementWait <= share.WorkerStopTimeout {
		t.Fatalf("management wait %s must exceed worker stop timeout %s", defaultManagementWait, share.WorkerStopTimeout)
	}
}

func TestCloudflaredAbsenceBlocksOnlyStart(t *testing.T) {
	app, stdout, _ := testApplication(t)
	app.lookPath = func(name string) (string, error) {
		if name == "cloudflared" {
			return "", errors.New("missing")
		}
		return "/opt/bin/" + name, nil
	}
	if err := app.start(""); err == nil || !strings.Contains(err.Error(), "cloudflared") {
		t.Fatalf("start error = %v", err)
	}
	if err := app.list(false); err != nil {
		t.Fatalf("list without cloudflared = %v", err)
	}
	if !strings.Contains(stdout.String(), "No running Wraps") {
		t.Fatalf("list output = %q", stdout.String())
	}
}

func TestStartDependenciesRejectOldVersions(t *testing.T) {
	for _, test := range []struct {
		name   string
		tmux   string
		cloud  string
		wanted string
	}{
		{name: "old tmux", tmux: "tmux 3.1", cloud: "cloudflared version 2026.7.0", wanted: "3.2"},
		{name: "old cloudflared", tmux: "tmux 3.5", cloud: "cloudflared version 2020.5.0", wanted: "2020.5.1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			app, _, _ := testApplication(t)
			app.runCommand = func(_ context.Context, _ string, args ...string) (string, error) {
				if len(args) == 1 && args[0] == "-V" {
					return test.tmux, nil
				}
				return test.cloud, nil
			}
			err := app.start("")
			if err == nil || !strings.Contains(err.Error(), test.wanted) {
				t.Fatalf("start dependency error = %v, want %q", err, test.wanted)
			}
		})
	}
}

func TestStartDependencyProbeHasDefaultTimeout(t *testing.T) {
	app, _, _ := testApplication(t)
	previous := externalProbeTimeout
	externalProbeTimeout = 20 * time.Millisecond
	t.Cleanup(func() { externalProbeTimeout = previous })
	app.runCommand = func(ctx context.Context, _ string, _ ...string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	started := time.Now()
	err := app.checkStartDependencies(context.Background())
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("checkStartDependencies() = %v after %s", err, time.Since(started))
	}
}

func TestDoctorLegacyProbeHonorsShorterCallerDeadline(t *testing.T) {
	app, _, _ := testApplication(t)
	legacyCalls := 0
	app.runCommand = func(ctx context.Context, _ string, args ...string) (string, error) {
		if len(args) == 1 && args[0] == "-V" {
			return "tmux 3.5a", nil
		}
		if len(args) == 1 && args[0] == "--version" {
			return "cloudflared version 2026.7.0", nil
		}
		legacyCalls++
		<-ctx.Done()
		return "", ctx.Err()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := app.doctorContext(ctx, false); err != nil {
		t.Fatal(err)
	}
	if legacyCalls != 2 || time.Since(started) > time.Second {
		t.Fatalf("legacy probes = %d after %s", legacyCalls, time.Since(started))
	}
}

func TestDoctorGivesStateProbeFreshTimeoutAfterDependencyTimeout(t *testing.T) {
	app, stdout, _ := testApplication(t)
	record := commandTestRecord(t, app)
	previous := externalProbeTimeout
	externalProbeTimeout = 20 * time.Millisecond
	t.Cleanup(func() { externalProbeTimeout = previous })
	app.runCommand = func(ctx context.Context, _ string, args ...string) (string, error) {
		if len(args) == 1 && args[0] == "-V" {
			<-ctx.Done()
			return "", ctx.Err()
		}
		if len(args) == 1 && args[0] == "--version" {
			return "cloudflared version 2026.7.0", nil
		}
		return "", errors.New("no legacy server")
	}
	app.callControl = func(ctx context.Context, _ string, _ control.Request) (control.Status, error) {
		if err := ctx.Err(); err != nil {
			return control.Status{}, err
		}
		return commandTestStatus(record), nil
	}
	if err := app.doctorContext(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "instances: 1 live, 0 stale, 0 unreachable") {
		t.Fatalf("doctor state after dependency timeout = %q", stdout.String())
	}
}

func TestBootstrapResolvesTmuxShellBeforeStartingShare(t *testing.T) {
	app, _, _ := testApplication(t)
	app.getenv = func(name string) string {
		if name == "TMUX" {
			return "/tmp/tmux.sock,123,0"
		}
		return ""
	}
	app.resolveShell = func(func(string) string, tmux.Runner) (string, error) {
		return "", errors.New("invalid tmux default-shell")
	}
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) {
		t.Fatal("target resolved before default shell")
		return target.Target{}, nil
	}
	app.startWorker = func(context.Context, string, instance.Record) (control.Status, error) {
		t.Fatal("share started before default shell")
		return control.Status{}, nil
	}
	err := app.bootstrap("api")
	if err == nil || !strings.Contains(err.Error(), "invalid tmux default-shell") {
		t.Fatalf("bootstrap() = %v", err)
	}
}

func TestBootstrapResolvesTmuxDefaultCommandBeforeStartingShare(t *testing.T) {
	app, _, _ := testApplication(t)
	app.getenv = func(name string) string {
		if name == "TMUX" {
			return "/tmp/tmux.sock,123,0"
		}
		return ""
	}
	app.resolveCommand = func(func(string) string, tmux.Runner) (string, error) {
		return "", errors.New("invalid tmux default-command")
	}
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) {
		t.Fatal("target resolved before default command")
		return target.Target{}, nil
	}
	app.startWorker = func(context.Context, string, instance.Record) (control.Status, error) {
		t.Fatal("share started before default command")
		return control.Status{}, nil
	}
	err := app.bootstrap("api")
	if err == nil || !strings.Contains(err.Error(), "invalid tmux default-command") {
		t.Fatalf("bootstrap() = %v", err)
	}
}

func TestBootstrapStartsShareThenExecsTmuxDefaultShell(t *testing.T) {
	app, _, _ := testApplication(t)
	app.getenv = func(name string) string {
		switch name {
		case "TMUX":
			return "/tmp/tmux.sock,123,0"
		case "SHELL":
			return "/bin/zsh"
		default:
			return ""
		}
	}
	app.resolveShell = func(func(string) string, tmux.Runner) (string, error) { return "/bin/sh", nil }
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) { return commandTestTarget(), nil }
	app.startWorker = func(_ context.Context, _ string, record instance.Record) (control.Status, error) {
		return commandTestStatus(record), nil
	}
	var execPath string
	var execArgs []string
	app.replaceProcess = func(path string, args, _ []string) error {
		execPath, execArgs = path, append([]string(nil), args...)
		return nil
	}
	if err := app.bootstrap("api"); err != nil {
		t.Fatal(err)
	}
	if execPath != "/bin/sh" || len(execArgs) != 1 || execArgs[0] != "-sh" {
		t.Fatalf("shell exec = %q %v", execPath, execArgs)
	}
}

func TestBootstrapStopsStartupSignalHandlingBeforeExec(t *testing.T) {
	app, _, stderr := testApplication(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopCalled := false
	app.startContext = func() (context.Context, context.CancelFunc) {
		return ctx, func() { stopCalled = true }
	}
	app.getenv = func(name string) string {
		if name == "TMUX" {
			return "/tmp/tmux.sock,123,0"
		}
		return ""
	}
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) {
		return commandTestTarget(), nil
	}
	app.startWorker = func(ctx context.Context, _ string, _ instance.Record) (control.Status, error) {
		if !errors.Is(ctx.Err(), context.Canceled) {
			t.Fatalf("worker startup context error = %v, want canceled", ctx.Err())
		}
		return control.Status{}, ctx.Err()
	}
	app.replaceProcess = func(string, []string, []string) error {
		if !stopCalled {
			t.Fatal("bootstrap exec replaced process before stopping signal handling")
		}
		return nil
	}
	if err := app.bootstrap(""); err != nil {
		t.Fatalf("bootstrap() = %v", err)
	}
	if !strings.Contains(stderr.String(), context.Canceled.Error()) {
		t.Fatalf("bootstrap error output = %q", stderr.String())
	}
}

func TestBootstrapStartsShareThenExecsTmuxDefaultCommand(t *testing.T) {
	app, _, _ := testApplication(t)
	app.getenv = func(name string) string {
		if name == "TMUX" {
			return "/tmp/tmux.sock,123,0"
		}
		return ""
	}
	app.resolveShell = func(func(string) string, tmux.Runner) (string, error) { return "/bin/sh", nil }
	app.resolveCommand = func(func(string) string, tmux.Runner) (string, error) {
		return "exec env -i /bin/zsh", nil
	}
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) { return commandTestTarget(), nil }
	app.startWorker = func(_ context.Context, _ string, record instance.Record) (control.Status, error) {
		return commandTestStatus(record), nil
	}
	var execPath string
	var execArgs []string
	app.replaceProcess = func(path string, args, _ []string) error {
		execPath, execArgs = path, append([]string(nil), args...)
		return nil
	}
	if err := app.bootstrap("api"); err != nil {
		t.Fatal(err)
	}
	if execPath != "/bin/sh" || strings.Join(execArgs, "\x00") != "/bin/sh\x00-c\x00exec env -i /bin/zsh" {
		t.Fatalf("default command exec = %q %v", execPath, execArgs)
	}
}

func TestBootstrapRollsBackShareWhenShellExecFails(t *testing.T) {
	app, _, _ := testApplication(t)
	app.getenv = func(name string) string {
		if name == "TMUX" {
			return "/tmp/tmux.sock,123,0"
		}
		return ""
	}
	app.resolveShell = func(func(string) string, tmux.Runner) (string, error) { return "/bin/sh", nil }
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) { return commandTestTarget(), nil }
	var launched instance.Record
	app.startWorker = func(_ context.Context, _ string, record instance.Record) (control.Status, error) {
		launched = record
		record.PID = os.Getpid()
		if err := app.store.Create(record); err != nil {
			t.Fatal(err)
		}
		return commandTestStatus(record), nil
	}
	shutdownCalled := false
	app.callControl = func(_ context.Context, _ string, request control.Request) (control.Status, error) {
		if request.InstanceID != launched.ID || request.Action != control.ActionShutdown {
			t.Fatalf("rollback request = %+v", request)
		}
		shutdownCalled = true
		if err := app.store.Remove(launched.ID); err != nil {
			t.Fatal(err)
		}
		return commandTestStatus(launched), nil
	}
	app.replaceProcess = func(string, []string, []string) error { return errors.New("exec failed") }
	err := app.bootstrap("api")
	if err == nil || !strings.Contains(err.Error(), "exec failed") {
		t.Fatalf("bootstrap() = %v", err)
	}
	if !shutdownCalled {
		t.Fatal("bootstrap did not roll back started share")
	}
	if _, err := app.store.Resolve(launched.ID); !errors.Is(err, instance.ErrNotFound) {
		t.Fatalf("started share record after rollback = %v", err)
	}
}

func TestBootstrapEntersShellWhenShareStartupFails(t *testing.T) {
	app, _, stderr := testApplication(t)
	app.getenv = func(name string) string {
		switch name {
		case "TMUX":
			return "/tmp/tmux.sock,123,0"
		case "SHELL":
			return "/bin/zsh"
		default:
			return ""
		}
	}
	app.resolveTarget = func(func(string) string, tmux.Runner) (target.Target, error) {
		return target.Target{}, errors.New("target vanished")
	}
	var shellCalled bool
	app.replaceProcess = func(string, []string, []string) error {
		shellCalled = true
		return nil
	}
	if err := app.bootstrap(""); err != nil {
		t.Fatalf("bootstrap() = %v", err)
	}
	if !shellCalled || !strings.Contains(stderr.String(), "target vanished") {
		t.Fatalf("failure shell/output = %v / %q", shellCalled, stderr.String())
	}
}
