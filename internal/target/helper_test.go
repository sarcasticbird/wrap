package target

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sarcasticbird/wrap/internal/tmux"
)

type helperRunner struct {
	outputs []string
	errors  []error
	calls   [][]string
}

type blockingContextRunner struct{}

func (blockingContextRunner) Run(...string) (string, error) {
	return "", errors.New("unbounded Run called")
}

func (blockingContextRunner) RunContext(ctx context.Context, _ ...string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (r *helperRunner) Run(args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	var output string
	if len(r.outputs) != 0 {
		output = r.outputs[0]
		r.outputs = r.outputs[1:]
	}
	var err error
	if len(r.errors) != 0 {
		err = r.errors[0]
		r.errors = r.errors[1:]
	}
	return output, err
}

func TestCreateHelperLinksOnlyCapturedWindow(t *testing.T) {
	t.Parallel()

	runner := &helperRunner{outputs: []string{"$8\t@99", "", "", "", "", ""}}
	target := Target{
		SocketPath: "/tmp/tmux-501/default",
		Generation: "0123456789abcdef0123456789abcdef",
		SessionID:  "$7",
		WindowID:   "@12",
	}
	helper, err := CreateHelper(target, "01JABCDEF234567890", runner)
	if err != nil {
		t.Fatalf("CreateHelper() error = %v", err)
	}
	if helper.SessionID != "$8" || helper.Name != "__wrap_01jabcdef234" {
		t.Fatalf("helper = %+v", helper)
	}
	joined := make([]string, len(runner.calls))
	for i, call := range runner.calls {
		joined[i] = strings.Join(call, " ")
		if len(call) < 2 || call[0] != "-S" || call[1] != target.SocketPath {
			t.Fatalf("call %d used wrong endpoint: %v", i, call)
		}
	}
	all := strings.Join(joined, "\n")
	for _, required := range []string{
		"new-session -d -s __wrap_01jabcdef234",
		"link-window -d -k",
		"$7:@12",
		"$8:@99",
		"@wrap_instance_id 01JABCDEF234567890",
		"prefix None",
		"prefix2 None",
		"status off",
		"destroy-unattached off",
		"key-table __wrap_keys_01jabcdef234567890",
	} {
		if !strings.Contains(all, required) {
			t.Fatalf("helper calls missing %q:\n%s", required, all)
		}
	}
	if strings.Contains(all, "new-session -d -t $7") {
		t.Fatalf("helper joined the source session group:\n%s", all)
	}
	if strings.Contains(all, "/bin/sleep") || !strings.Contains(all, "sleep 300") {
		t.Fatalf("helper placeholder command does not resolve sleep through PATH:\n%s", all)
	}
}

func TestHelperCloseRequiresOwnedMarker(t *testing.T) {
	t.Parallel()

	runner := &helperRunner{}
	helper := &Helper{
		SessionID:  "$8",
		Name:       "__wrap_01jabcdef234",
		InstanceID: "01JABCDEF234567890",
		Target: Target{
			SocketPath: "/tmp/tmux-501/default",
			Generation: "0123456789abcdef0123456789abcdef",
		},
		run: runner,
	}
	if err := helper.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("close calls = %d, want 1", len(runner.calls))
	}
	call := strings.Join(runner.calls[0], " ")
	for _, required := range []string{
		"#{==:#{@wrap_server_generation},0123456789abcdef0123456789abcdef}",
		"#{==:#{session_name},__wrap_01jabcdef234}",
		"#{==:#{@wrap_instance_id},01JABCDEF234567890}",
		"kill-session -t $8",
	} {
		if !strings.Contains(call, required) {
			t.Fatalf("guarded close missing %q: %s", required, call)
		}
	}
}

func TestCleanupHelperDiscoversAndKillsOnlyExactStaleOwner(t *testing.T) {
	t.Parallel()
	const generation = "0123456789abcdef0123456789abcdef"
	runner := &helperRunner{outputs: []string{
		generation + "\t$8\t__wrap_01jabcdef234\t01JABCDEF234567890\t@12",
		"",
	}}
	target := Target{SocketPath: "/tmp/tmux-501/default", Generation: generation, WindowID: "@12"}
	if err := CleanupHelper(target, "01JABCDEF234567890", runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 {
		t.Fatalf("cleanup calls = %d, want inspect and guarded kill", len(runner.calls))
	}
	if call := strings.Join(runner.calls[1], " "); !strings.Contains(call, "kill-session -t $8") ||
		!strings.Contains(call, "@wrap_instance_id},01JABCDEF234567890") {
		t.Fatalf("guarded cleanup = %s", call)
	}
}

func TestCleanupHelperTreatsConfirmedMissingTargetAsClean(t *testing.T) {
	t.Parallel()
	runner := &helperRunner{errors: []error{tmux.ErrMissingTarget}}
	err := CleanupHelper(Target{
		SocketPath: "/tmp/tmux-501/default",
		Generation: "0123456789abcdef0123456789abcdef",
		WindowID:   "@12",
	}, "01JABCDEF234567890", runner)
	if err != nil {
		t.Fatalf("CleanupHelper() = %v", err)
	}
}

func TestHelperValidateRequiresExactOwnedWindow(t *testing.T) {
	t.Parallel()

	const generation = "0123456789abcdef0123456789abcdef"
	runner := &helperRunner{outputs: []string{
		generation + "\t$8\t__wrap_01jabcdef234\t01JABCDEF234567890\t@12",
		generation + "\t$7\t@12",
		generation + "\t$8\t__wrap_01jabcdef234\t01JABCDEF234567890\t@99",
	}}
	helper := &Helper{
		SessionID:  "$8",
		Name:       "__wrap_01jabcdef234",
		InstanceID: "01JABCDEF234567890",
		Target: Target{
			SocketPath: "/tmp/tmux-501/default",
			Generation: generation,
			SessionID:  "$7",
			WindowID:   "@12",
		},
		run: runner,
	}
	if err := helper.Validate(t.Context()); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	if err := helper.Validate(t.Context()); err == nil || !strings.Contains(err.Error(), "identity changed") {
		t.Fatalf("Validate() stale window = %v", err)
	}
	if got := strings.Join(runner.calls[0], " "); !strings.Contains(got, "display-message -p -t $8") {
		t.Fatalf("Validate() call = %s", got)
	}
}

func TestHelperValidateHonorsContext(t *testing.T) {
	helper := &Helper{
		SessionID: "$8", Name: "__wrap_01jabcdef234", InstanceID: "01JABCDEF234567890",
		Target: Target{
			SocketPath: "/tmp/tmux-501/default",
			Generation: "0123456789abcdef0123456789abcdef",
			SessionID:  "$7",
			WindowID:   "@12",
		},
		run: blockingContextRunner{},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if err := helper.Validate(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Validate() = %v, want deadline exceeded", err)
	}
}

func TestCreateHelperCleansCreatedSessionWhenMarkingFails(t *testing.T) {
	t.Parallel()

	runner := &helperRunner{
		outputs: []string{"$8\t@99", "", "", ""},
		errors:  []error{nil, nil, errors.New("mark failed"), nil},
	}
	_, err := CreateHelper(Target{
		SocketPath: "/tmp/tmux-501/default",
		Generation: "0123456789abcdef0123456789abcdef",
		SessionID:  "$7",
		WindowID:   "@12",
	}, "01JABCDEF234567890", runner)
	if err == nil || !strings.Contains(err.Error(), "mark failed") {
		t.Fatalf("CreateHelper() = %v", err)
	}
	if len(runner.calls) != 4 {
		t.Fatalf("helper calls = %d, want create, link, mark, cleanup", len(runner.calls))
	}
	cleanup := strings.Join(runner.calls[3], " ")
	for _, want := range []string{"session_name},__wrap_01jabcdef234", "kill-session -t $8"} {
		if !strings.Contains(cleanup, want) {
			t.Fatalf("cleanup missing %q: %s", want, cleanup)
		}
	}
}

func TestGroupedHelperDoesNotMoveOrKillSourceSession(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	_ = tmuxPath

	socketPath := filepath.Join(
		"/tmp",
		fmt.Sprintf("wrap-helper-test-%d-%d.sock", os.Getpid(), time.Now().UnixNano()),
	)
	server := tmux.NewServerPath(socketPath)
	t.Cleanup(func() {
		_, _ = server.Run("kill-server")
	})
	if _, err := server.Run("new-session", "-d", "-s", "source", "-n", "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("new-window", "-d", "-t", "source", "-n", "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("select-window", "-t", "source:two"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("set-option", "-t", "source", "destroy-unattached", "off"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("set-option", "-g", "destroy-unattached", "on"); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := server.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	identity, err := server.Run(
		"display-message", "-p", "-t", "source:two",
		"#{session_id}\t#{window_id}",
	)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(identity, "\t")
	if len(parts) != 2 {
		t.Fatalf("target identity = %q", identity)
	}
	if _, err := server.Run("bind-key", "-n", "C-g", "switch-client", "-t", "source"); err != nil {
		t.Fatal(err)
	}
	instanceID := fmt.Sprintf("integration-%d", os.Getpid())
	helper, err := CreateHelper(Target{
		SocketPath: socketPath,
		Generation: generation,
		SessionID:  parts[0],
		WindowID:   parts[1],
	}, instanceID, nil)
	if err != nil {
		t.Fatal(err)
	}
	destroyUnattached, err := server.Run("show-options", "-v", "-t", helper.SessionID, "destroy-unattached")
	if err != nil || destroyUnattached != "off" {
		t.Fatalf("helper destroy-unattached = %q, %v", destroyUnattached, err)
	}
	keyTable, err := server.Run("show-options", "-v", "-t", helper.SessionID, "key-table")
	if err != nil || keyTable != "__wrap_keys_"+strings.ToLower(instanceID) {
		t.Fatalf("helper key-table = %q, %v", keyTable, err)
	}
	rootKeys, err := server.Run("list-keys", "-T", "root")
	if err != nil || !strings.Contains(rootKeys, "switch-client") {
		t.Fatalf("root switch-client binding = %q, %v", rootKeys, err)
	}
	if helperKeys, err := server.Run("list-keys", "-T", keyTable); err == nil || strings.TrimSpace(helperKeys) != "" {
		t.Fatalf("isolated helper key table unexpectedly exists: %q, %v", helperKeys, err)
	}
	if _, err := server.Run("select-window", "-t", "source:one"); err != nil {
		t.Fatal(err)
	}
	selections, err := server.Run("list-sessions", "-F", "#{session_name}\t#{window_name}")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(selections, "source\tone") || !strings.Contains(selections, helper.Name+"\ttwo") {
		t.Fatalf("independent selections missing:\n%s", selections)
	}
	if err := helper.Validate(t.Context()); err != nil {
		t.Fatalf("Validate() while source owns window = %v", err)
	}
	if err := helper.Close(); err != nil {
		t.Fatal(err)
	}
	remaining, err := server.Run("list-sessions", "-F", "#{session_name}\t#{window_name}")
	if err != nil {
		t.Fatal(err)
	}
	if remaining != "source\tone" {
		t.Fatalf("source after helper cleanup = %q", remaining)
	}
}

func TestHelperHasNoFallbackWhenCapturedWindowIsDestroyed(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socketPath := filepath.Join(
		"/tmp",
		fmt.Sprintf("wrap-helper-fallback-test-%d-%d.sock", os.Getpid(), time.Now().UnixNano()),
	)
	server := tmux.NewServerPath(socketPath)
	t.Cleanup(func() { _, _ = server.Run("kill-server") })
	if _, err := server.Run("new-session", "-d", "-s", "source", "-n", "one"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("new-window", "-d", "-t", "source", "-n", "captured"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("select-window", "-t", "source:captured"); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := server.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	identity, err := server.Run("display-message", "-p", "-t", "source", "#{session_id}\t#{window_id}")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(identity, "\t")
	helper, err := CreateHelper(Target{
		SocketPath: socketPath, Generation: generation, SessionID: parts[0], WindowID: parts[1],
	}, fmt.Sprintf("fallback-%d", os.Getpid()), nil)
	if err != nil {
		t.Fatal(err)
	}
	windows, err := server.Run("list-windows", "-t", helper.SessionID, "-F", "#{window_id}")
	if err != nil || strings.TrimSpace(windows) != parts[1] {
		t.Fatalf("helper windows = %q, %v", windows, err)
	}
	if _, err := server.Run("kill-window", "-t", parts[1]); err != nil {
		t.Fatal(err)
	}
	if exists, err := server.HasSession(helper.Name); err != nil || exists {
		t.Fatalf("helper after captured window destruction = exists:%v error:%v", exists, err)
	}
}

func TestHelperValidateFailsWhenSourceSessionEnds(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socketPath := filepath.Join(
		"/tmp",
		fmt.Sprintf("wrap-helper-source-test-%d-%d.sock", os.Getpid(), time.Now().UnixNano()),
	)
	server := tmux.NewServerPath(socketPath)
	t.Cleanup(func() { _, _ = server.Run("kill-server") })
	if _, err := server.Run("new-session", "-d", "-s", "source", "-n", "captured"); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := server.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	identity, err := server.Run("display-message", "-p", "-t", "source", "#{session_id}\t#{window_id}")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(identity, "\t")
	helper, err := CreateHelper(Target{
		SocketPath: socketPath, Generation: generation, SessionID: parts[0], WindowID: parts[1],
	}, fmt.Sprintf("source-end-%d", os.Getpid()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("kill-session", "-t", "source"); err != nil {
		t.Fatal(err)
	}
	if err := helper.Validate(t.Context()); err == nil || !strings.Contains(err.Error(), "source tmux window") {
		t.Fatalf("Validate() after source ended = %v", err)
	}
	if err := helper.Close(); err != nil {
		t.Fatal(err)
	}
}
