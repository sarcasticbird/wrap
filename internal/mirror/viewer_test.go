package mirror

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sarcasticbird/wrap/internal/tmux"
)

var viewerTestCounter atomic.Uint64

func TestPTYViewerFactoryBuildsGenerationGuardedIgnoredSizeAttach(t *testing.T) {
	factory := PTYViewerFactory{
		SessionSocket: "wrap-test",
		TmuxPath:      "/usr/bin/tmux",
		Environment: []string{
			"PATH=/usr/bin",
			"TERM=screen-256color",
			"TMUX=/tmp/tmux,1,0",
			"TMUX_PANE=%2",
			"LANG=en_US.UTF-8",
		},
	}
	command, err := factory.buildCommand(Identity{
		ID:         "$7",
		Generation: "0123456789abcdef0123456789abcdef",
	}, "@4")
	if err != nil {
		t.Fatal(err)
	}
	if command.Path != "/usr/bin/tmux" {
		t.Fatalf("command path = %q", command.Path)
	}
	joined := strings.Join(command.Args, " ")
	if !strings.Contains(joined, "-L wrap-test if-shell") ||
		!strings.Contains(joined, "#{==:#{window_id},@4}") ||
		!strings.Contains(joined, "attach-session -f ignore-size -t $7") {
		t.Fatalf("viewer command = %q", joined)
	}
	gotEnv := strings.Join(command.Env, "\n")
	if strings.Contains(gotEnv, "TMUX=") || strings.Contains(gotEnv, "TMUX_PANE=") {
		t.Fatalf("viewer inherited tmux environment:\n%s", gotEnv)
	}
	if !strings.Contains(gotEnv, "TERM=xterm-256color") ||
		!strings.Contains(gotEnv, "LANG=en_US.UTF-8") {
		t.Fatalf("viewer environment =\n%s", gotEnv)
	}
}

func TestIntentionalViewerTerminationSuppressesKilledExit(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 7")
	err := command.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("fixture error = %v", err)
	}
	if got := viewerWaitError(err, true); got != nil {
		t.Fatalf("intentional wait error = %v", got)
	}
	if got := viewerWaitError(err, false); !errors.Is(got, err) {
		t.Fatalf("unexpected-exit error = %v", got)
	}
}

func TestViewerExitResultTreatsMissingTerminalAsGraceful(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 1")
	waitErr := command.Run()
	if waitErr == nil {
		t.Fatal("fixture command unexpectedly succeeded")
	}

	probeCalls := 0
	got := viewerExitResult(waitErr, false, func() (bool, error) {
		probeCalls++
		return true, nil
	})
	if got != nil {
		t.Fatalf("missing-terminal exit result = %v", got)
	}
	if probeCalls != 1 {
		t.Fatalf("missing-terminal probe calls = %d, want 1", probeCalls)
	}
}

func TestViewerExitResultPreservesUnexpectedExitAndProbeError(t *testing.T) {
	command := exec.Command("sh", "-c", "exit 1")
	waitErr := command.Run()
	if waitErr == nil {
		t.Fatal("fixture command unexpectedly succeeded")
	}
	probeErr := errors.New("probe failed")

	got := viewerExitResult(waitErr, false, func() (bool, error) {
		return false, probeErr
	})
	if !errors.Is(got, waitErr) || !errors.Is(got, probeErr) {
		t.Fatalf("unexpected-exit result = %v", got)
	}
}

func TestPTYViewerFactoryRejectsInvalidIdentityAndSize(t *testing.T) {
	factory := PTYViewerFactory{TmuxPath: "/usr/bin/tmux", SessionSocket: "wrap-test"}
	if _, err := factory.buildCommand(
		Identity{Generation: "generation"},
		"@4",
	); err == nil {
		t.Fatal("viewer accepted empty session id")
	}
	for _, size := range [][2]uint16{{1, 24}, {501, 24}, {80, 1}, {80, 301}} {
		if _, err := factory.Open(t.Context(), Identity{
			ID: "$7", Generation: "generation",
		}, size[0], size[1], func([]byte) error { return nil }); err == nil {
			t.Errorf("viewer accepted size %dx%d", size[0], size[1])
		}
	}
}

func TestViewerIntegrationUsesPTYWithoutResizingAndRefusesRestartedServer(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf(
		"wrap-mirror-test-%d-%d",
		os.Getpid(),
		viewerTestCounter.Add(1),
	)
	server := tmux.NewServer(socket)
	server.ConfigFile = os.DevNull
	t.Cleanup(func() {
		_, _ = server.Run("kill-server")
	})
	if err := server.NewSession("viewer", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := server.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	sessions, err := server.Sessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("test sessions = %+v, %v", sessions, err)
	}
	identity := Identity{ID: sessions[0].ID, Generation: generation}
	sizeBefore, err := server.Run(
		"display-message", "-p", "-t", identity.ID, "#{window_width}x#{window_height}",
	)
	if err != nil {
		t.Fatal(err)
	}
	var outputMu sync.Mutex
	var output strings.Builder
	outputReady := make(chan struct{}, 1)
	factory := PTYViewerFactory{
		SessionSocket: socket,
		TmuxPath:      tmuxPath,
	}
	viewer, err := factory.Open(t.Context(), identity, 30, 10, func(chunk []byte) error {
		outputMu.Lock()
		_, _ = output.Write(chunk)
		found := strings.Contains(output.String(), "wrap-mirror-viewer-sentinel")
		outputMu.Unlock()
		if found {
			select {
			case outputReady <- struct{}{}:
			default:
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := viewer.Write([]byte("printf 'wrap-mirror-viewer-sentinel\\n'\n")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-outputReady:
	case <-time.After(3 * time.Second):
		outputMu.Lock()
		got := output.String()
		outputMu.Unlock()
		t.Fatalf("timed out waiting for viewer output; got %q", got)
	}
	sizeAfter, err := server.Run(
		"display-message", "-p", "-t", identity.ID, "#{window_width}x#{window_height}",
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(sizeAfter) != strings.TrimSpace(sizeBefore) {
		t.Fatalf("ignored-size viewer changed shared window from %q to %q", sizeBefore, sizeAfter)
	}
	secondViewer, err := factory.Open(
		t.Context(),
		identity,
		40,
		12,
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := viewer.Close(); err != nil {
		t.Fatal(err)
	}
	modeDuring, err := server.Run("show-options", "-w", "-v", "-A", "-t", identity.ID, "window-size")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(modeDuring) != "manual" {
		t.Fatalf("first viewer close restored mode with another viewer active: %q", modeDuring)
	}
	if err := secondViewer.Close(); err != nil {
		t.Fatal(err)
	}
	modeAfter, err := server.Run("show-options", "-w", "-v", "-t", identity.ID, "window-size")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(modeAfter) != "" {
		t.Fatalf("viewer left inherited window-size as a local override: %q", modeAfter)
	}
	effectiveMode, err := server.Run(
		"show-options", "-w", "-v", "-A", "-t", identity.ID, "window-size",
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(effectiveMode) != "latest" {
		t.Fatalf("viewer restored effective window-size mode to %q, want latest", effectiveMode)
	}

	endingViewer, err := factory.Open(
		t.Context(),
		identity,
		30,
		10,
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("kill-session", "-t", identity.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case viewerErr := <-endingViewer.Done():
		if viewerErr != nil {
			t.Fatalf("viewer terminal-end result = %v", viewerErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("viewer did not exit after its terminal ended")
	}
	if err := endingViewer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := server.NewSession("replacement", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const replacementGeneration = "fedcba9876543210fedcba9876543210"
	if _, err := server.EnsureServerGeneration(replacementGeneration); err != nil {
		t.Fatal(err)
	}
	replacement, err := server.Sessions()
	if err != nil || len(replacement) != 1 {
		t.Fatalf("replacement sessions = %+v, %v", replacement, err)
	}
	if replacement[0].ID != identity.ID {
		t.Fatalf("test server did not reuse identity: old=%s new=%s", identity.ID, replacement[0].ID)
	}
	_, err = factory.Open(
		context.Background(),
		identity,
		30,
		10,
		func([]byte) error { return nil },
	)
	if !errors.Is(err, tmux.ErrServerGenerationChanged) {
		t.Fatalf("generation-mismatched viewer = %v, want ErrServerGenerationChanged", err)
	}
}

func TestParseWindowPinPreservesLocalAndInheritedModes(t *testing.T) {
	for _, test := range []struct {
		name      string
		result    string
		windowID  string
		mode      string
		inherited bool
	}{
		{"local", "@4\nwindow-size largest\n", "@4", "largest", false},
		{"inherited", "@9\nwindow-size* latest\n", "@9", "latest", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			windowID, mode, inherited, err := parseWindowPin(test.result)
			if err != nil {
				t.Fatal(err)
			}
			if windowID != test.windowID || mode != test.mode || inherited != test.inherited {
				t.Fatalf(
					"parseWindowPin = %q, %q, %v",
					windowID,
					mode,
					inherited,
				)
			}
		})
	}
	for _, invalid := range []string{
		"",
		"$4\nwindow-size latest\n",
		"@4\nwindow-size unsafe\n",
		"@4\nunknown latest\n",
	} {
		if _, _, _, err := parseWindowPin(invalid); err == nil {
			t.Fatalf("parseWindowPin accepted %q", invalid)
		}
	}
}

func TestViewerWindowPinIsSharedByLinkedSessions(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf(
		"wrap-mirror-linked-test-%d-%d",
		os.Getpid(),
		viewerTestCounter.Add(1),
	)
	server := tmux.NewServer(socket)
	server.ConfigFile = os.DevNull
	t.Cleanup(func() {
		_, _ = server.Run("kill-server")
	})
	if err := server.NewSession("target", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("new-session", "-d", "-s", "alias", "-t", "target"); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := server.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	sessions, err := server.Sessions()
	if err != nil || len(sessions) != 2 {
		t.Fatalf("linked sessions = %+v, %v", sessions, err)
	}
	identities := make([]Identity, 0, len(sessions))
	for _, session := range sessions {
		identities = append(identities, Identity{
			ID: session.ID, Generation: generation,
		})
	}
	factory := PTYViewerFactory{SessionSocket: socket, TmuxPath: tmuxPath}
	first, err := factory.Open(
		t.Context(),
		identities[0],
		80,
		24,
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	second, err := factory.Open(
		t.Context(),
		identities[1],
		40,
		12,
		func([]byte) error { return nil },
	)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	factory.pinMu.Lock()
	if len(factory.pins) != 1 {
		factory.pinMu.Unlock()
		t.Fatalf("linked sessions created %d window pins, want 1", len(factory.pins))
	}
	var references int
	for _, pin := range factory.pins {
		references = pin.references
	}
	factory.pinMu.Unlock()
	if references != 2 {
		t.Fatalf("linked-window pin references = %d, want 2", references)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	factory.pinMu.Lock()
	remaining := len(factory.pins)
	factory.pinMu.Unlock()
	if remaining != 0 {
		t.Fatalf("linked-window pins after last close = %d", remaining)
	}
}
