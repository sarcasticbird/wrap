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

	"github.com/creack/pty"
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

func TestPTYViewerFactoryRejectsInvalidIdentityAndGeometry(t *testing.T) {
	factory := PTYViewerFactory{TmuxPath: "/usr/bin/tmux", SessionSocket: "wrap-test"}
	if _, err := factory.buildCommand(
		Identity{Generation: "generation"},
		"@4",
	); err == nil {
		t.Fatal("viewer accepted empty session id")
	}
	for _, geometry := range []string{"1\t24", "501\t24", "80\t1", "80\t301"} {
		if _, _, _, _, err := parseWindowPin("@4\t" + geometry + "\nwindow-size latest\noff\n"); err == nil {
			t.Errorf("viewer accepted geometry %s", geometry)
		}
	}
}

func TestParseViewerClientGeometrySelectsSpawnedClient(t *testing.T) {
	result := strings.Join([]string{
		"/dev/pts/7\t4242\t160\t50\tignore-size\t@4\t160\t49\t0\t1",
		"/dev/pts/8\t5151\t80\t24\t\t@9\t80\t23\t0\t1",
	}, "\n")
	got, err := parseViewerClientGeometry(result, 4242)
	if err != nil {
		t.Fatal(err)
	}
	want := viewerClientGeometry{
		name:          "/dev/pts/7",
		pid:           4242,
		columns:       160,
		rows:          50,
		flags:         "ignore-size",
		windowID:      "@4",
		windowColumns: 160,
		windowRows:    49,
		windowBigger:  false,
		statusRows:    1,
	}
	if got != want {
		t.Fatalf("viewer client geometry = %+v, want %+v", got, want)
	}
}

func TestParseViewerClientGeometryRejectsInvalidResults(t *testing.T) {
	for _, test := range []struct {
		name   string
		result string
		pid    int
	}{
		{name: "missing pid", result: "/dev/pts/7\t4242\t160\t50\tignore-size\t@4\t160\t49\t0\t1", pid: 5151},
		{name: "malformed", result: "broken", pid: 4242},
		{name: "invalid window", result: "/dev/pts/7\t4242\t160\t50\tignore-size\t4\t160\t49\t0\t1", pid: 4242},
		{name: "overflow", result: "/dev/pts/7\t4242\t65536\t50\tignore-size\t@4\t160\t49\t0\t1", pid: 4242},
		{name: "invalid status", result: "/dev/pts/7\t4242\t160\t50\tignore-size\t@4\t160\t49\t0\t6", pid: 4242},
		{name: "duplicate pid", result: strings.Join([]string{
			"/dev/pts/7\t4242\t160\t50\tignore-size\t@4\t160\t49\t0\t1",
			"/dev/pts/8\t4242\t160\t50\tignore-size\t@4\t160\t49\t0\t1",
		}, "\n"), pid: 4242},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := parseViewerClientGeometry(test.result, test.pid); err == nil {
				t.Fatal("invalid viewer client geometry was accepted")
			}
		})
	}
}

func TestViewerGeometryMatchesPinnedWindowExactly(t *testing.T) {
	captured := ViewerGeometry{Columns: 160, Rows: 50, statusRows: 1}
	exact := viewerClientGeometry{
		columns: 160, rows: 50, flags: "ignore-size", windowID: "@4", windowColumns: 160, windowRows: 49, statusRows: 1,
	}
	for _, test := range []struct {
		name   string
		client viewerClientGeometry
		want   bool
	}{
		{name: "exact", client: exact, want: true},
		{name: "client taller", client: func() viewerClientGeometry { value := exact; value.rows = 80; return value }()},
		{name: "window changed", client: func() viewerClientGeometry { value := exact; value.windowColumns = 159; return value }()},
		{name: "status mismatch", client: func() viewerClientGeometry { value := exact; value.windowRows = 48; return value }()},
		{name: "effective status changed", client: func() viewerClientGeometry { value := exact; value.statusRows = 2; return value }()},
		{name: "window bigger", client: func() viewerClientGeometry { value := exact; value.windowBigger = true; return value }()},
		{name: "size-affecting client", client: func() viewerClientGeometry { value := exact; value.flags = ""; return value }()},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := viewerGeometryMatches(captured, "@4", test.client); got != test.want {
				t.Fatalf("viewerGeometryMatches = %v, want %v", got, test.want)
			}
		})
	}
}

func TestVerifyViewerGeometryCorrectsOnceAndConverges(t *testing.T) {
	captured := ViewerGeometry{Columns: 160, Rows: 50, statusRows: 1}
	responses := []viewerClientGeometry{
		{
			name: "/dev/pts/7", pid: 4242, columns: 160, rows: 80,
			flags: "ignore-size", windowID: "@4", windowColumns: 160, windowRows: 49, statusRows: 1,
		},
		{
			name: "/dev/pts/7", pid: 4242, columns: 160, rows: 50,
			flags: "ignore-size", windowID: "@4", windowColumns: 160, windowRows: 49, statusRows: 1,
		},
	}
	queries := 0
	resizes := 0
	refreshes := 0
	var events []DiagnosticRecord
	factory := PTYViewerFactory{
		queryClient: func(pid int) (viewerClientGeometry, error) {
			if pid != 4242 {
				t.Fatalf("query pid = %d", pid)
			}
			value := responses[min(queries, len(responses)-1)]
			queries++
			return value, nil
		},
		resizePTY: func(_ *os.File, got ViewerGeometry) error {
			resizes++
			if got != captured {
				t.Fatalf("resize geometry = %+v, want %+v", got, captured)
			}
			return nil
		},
		refreshClient: func(name string) error {
			refreshes++
			if name != "/dev/pts/7" {
				t.Fatalf("refresh client = %q", name)
			}
			return nil
		},
		waitGeometry: func(context.Context, time.Duration) error { return nil },
		Record:       func(record DiagnosticRecord) { events = append(events, record) },
	}
	report, err := factory.verifyViewerGeometry(t.Context(), 4242, nil, "@4", captured)
	if err != nil {
		t.Fatal(err)
	}
	if queries != 2 || resizes != 1 || refreshes != 1 {
		t.Fatalf("geometry operations = queries:%d resizes:%d refreshes:%d", queries, resizes, refreshes)
	}
	if !report.Corrected || report.ClientRows != 50 || report.WindowRows != 49 {
		t.Fatalf("geometry report = %+v", report)
	}
	if len(events) != 3 || events[0].Event != "geometry_preparing" ||
		events[1].Event != "geometry_corrected" || events[2].Event != "geometry_verified" {
		t.Fatalf("geometry events = %+v", events)
	}
}

func TestVerifyViewerGeometryFailsAfterOneCorrection(t *testing.T) {
	captured := ViewerGeometry{Columns: 160, Rows: 50, statusRows: 1}
	client := viewerClientGeometry{
		name: "/dev/pts/7", pid: 4242, columns: 160, rows: 80,
		flags: "ignore-size", windowID: "@4", windowColumns: 160, windowRows: 49, statusRows: 1,
	}
	resizes := 0
	var events []DiagnosticRecord
	factory := PTYViewerFactory{
		queryClient: func(int) (viewerClientGeometry, error) { return client, nil },
		resizePTY: func(*os.File, ViewerGeometry) error {
			resizes++
			return nil
		},
		refreshClient: func(string) error { return nil },
		waitGeometry:  func(context.Context, time.Duration) error { return nil },
		Record:        func(record DiagnosticRecord) { events = append(events, record) },
	}
	if _, err := factory.verifyViewerGeometry(t.Context(), 4242, nil, "@4", captured); err == nil ||
		!strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("geometry timeout error = %v", err)
	}
	if resizes != 1 {
		t.Fatalf("geometry correction count = %d, want 1", resizes)
	}
	if got := events[len(events)-1].Event; got != "geometry_failed" {
		t.Fatalf("final geometry event = %q", got)
	}
}

func TestVerifyViewerGeometryAllowsConvergenceAfterLateAttachment(t *testing.T) {
	captured := ViewerGeometry{Columns: 160, Rows: 50, statusRows: 1}
	mismatch := viewerClientGeometry{
		name: "/dev/pts/7", pid: 4242, columns: 160, rows: 80,
		flags: "ignore-size", windowID: "@4", windowColumns: 160, windowRows: 49, statusRows: 1,
	}
	exact := mismatch
	exact.rows = 50
	queries := 0
	factory := PTYViewerFactory{
		queryClient: func(int) (viewerClientGeometry, error) {
			queries++
			switch {
			case queries < viewerGeometryAttempts:
				return viewerClientGeometry{}, errViewerClientNotAttached
			case queries == viewerGeometryAttempts:
				return mismatch, nil
			default:
				return exact, nil
			}
		},
		resizePTY:     func(*os.File, ViewerGeometry) error { return nil },
		refreshClient: func(string) error { return nil },
		waitGeometry:  func(context.Context, time.Duration) error { return nil },
	}
	if _, err := factory.verifyViewerGeometry(t.Context(), 4242, nil, "@4", captured); err != nil {
		t.Fatal(err)
	}
	if queries != viewerGeometryAttempts+1 {
		t.Fatalf("geometry queries = %d, want %d", queries, viewerGeometryAttempts+1)
	}
}

func TestVerifyViewerGeometryAllowsSlowAttachment(t *testing.T) {
	captured := ViewerGeometry{Columns: 160, Rows: 50, statusRows: 1}
	exact := viewerClientGeometry{
		name: "/dev/pts/7", pid: 4242, columns: 160, rows: 50,
		flags: "ignore-size", windowID: "@4", windowColumns: 160, windowRows: 49, statusRows: 1,
	}
	const delayedQueries = 50
	queries := 0
	factory := PTYViewerFactory{
		queryClient: func(int) (viewerClientGeometry, error) {
			queries++
			if queries <= delayedQueries {
				return viewerClientGeometry{}, errViewerClientNotAttached
			}
			return exact, nil
		},
		waitGeometry: func(context.Context, time.Duration) error { return nil },
	}
	if _, err := factory.verifyViewerGeometry(t.Context(), 4242, nil, "@4", captured); err != nil {
		t.Fatal(err)
	}
	if queries != delayedQueries+1 {
		t.Fatalf("geometry queries = %d, want %d", queries, delayedQueries+1)
	}
}

func TestPreparedViewerStartFailureReleasesWindowPinOnce(t *testing.T) {
	var releases atomic.Int32
	prepared := &ptyViewerPreparation{
		factory: &PTYViewerFactory{
			TmuxPath: "/definitely/missing/tmux", SessionSocket: "wrap-test",
		},
		ctx:      t.Context(),
		identity: Identity{ID: "$7", Generation: "0123456789abcdef0123456789abcdef"},
		windowID: "@4",
		geometry: ViewerGeometry{Columns: 80, Rows: 24},
		output:   func([]byte) error { return nil },
		releasePin: func() error {
			releases.Add(1)
			return nil
		},
	}
	if _, err := prepared.Start(); err == nil {
		t.Fatal("prepared viewer unexpectedly started with a missing tmux binary")
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("window pin releases = %d, want 1", got)
	}
}

func TestPinWindowDoesNotMutateWhenGeometryIsRejected(t *testing.T) {
	var calls [][]string
	factory := PTYViewerFactory{
		run: func(args []string) (string, error) {
			calls = append(calls, append([]string(nil), args...))
			if len(calls) == 1 {
				return "@4\t501\t24\nwindow-size* latest\non\n", nil
			}
			return "", nil
		},
	}
	_, _, _, err := factory.pinWindow(Identity{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef",
	})
	if err == nil || !strings.Contains(err.Error(), "columns 501") {
		t.Fatalf("invalid geometry error = %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("tmux calls = %d, want capture only", len(calls))
	}
	if capture := strings.Join(calls[0], " "); strings.Contains(capture, "set-option") {
		t.Fatalf("invalid capture mutated tmux state: %q", capture)
	}
}

func TestRestoreWindowPinIgnoresExitedTmuxServer(t *testing.T) {
	factory := PTYViewerFactory{
		run: func([]string) (string, error) {
			return "", errors.New("server exited unexpectedly: exit status 1")
		},
	}
	err := factory.restoreWindowPin(
		viewerWindowKey{
			generation: "0123456789abcdef0123456789abcdef",
			windowID:   "@4",
		},
		"latest",
		true,
		"00112233445566778899aabbccddeeff",
		424242,
	)
	if err != nil {
		t.Fatalf("restore after tmux server exit = %v, want nil", err)
	}
}

func TestPinWindowRetriesWhenCapturedStateChangesBeforeMutation(t *testing.T) {
	var calls [][]string
	factory := PTYViewerFactory{
		run: func(args []string) (string, error) {
			calls = append(calls, append([]string(nil), args...))
			switch len(calls) {
			case 1:
				return "@4\t132\t41\nwindow-size largest\non\n", nil
			case 2:
				return "wrap-window-pin-mismatch", nil
			case 3:
				return "@4\t140\t45\nwindow-size* latest\noff\n", nil
			default:
				return "", nil
			}
		},
	}
	windowID, geometry, release, err := factory.pinWindow(Identity{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if windowID != "@4" || geometry.Columns != 140 || geometry.Rows != 45 {
		t.Fatalf("retried pin = %q %+v, want @4 140x45", windowID, geometry)
	}
	if len(calls) != 4 {
		t.Fatalf("tmux calls = %d, want capture/pin twice", len(calls))
	}
	secondPin := strings.Join(calls[3], " ")
	for _, want := range []string{
		"#{==:#{window_width},140}",
		"#{==:#{window_height},45}",
		"#{==:#{window-size},latest}",
		"#{==:#{status},off}",
		"set-option -w -o -t @4 window-size manual",
	} {
		if !strings.Contains(secondPin, want) {
			t.Fatalf("retried pin command %q missing %q", secondPin, want)
		}
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	restore := strings.Join(calls[len(calls)-1], " ")
	if !strings.Contains(restore, "set-option -w -u -t @4 window-size") {
		t.Fatalf("retried pin restore = %q, want inherited-mode removal", restore)
	}
}

func TestPinWindowRetriesInheritedToLocalTransitionWithoutOverwritingLocalOption(t *testing.T) {
	var calls [][]string
	factory := PTYViewerFactory{
		run: func(args []string) (string, error) {
			calls = append(calls, append([]string(nil), args...))
			switch len(calls) {
			case 1:
				return "@4\t132\t41\nwindow-size* latest\non\n", nil
			case 2:
				return "", errors.New("already set: window-size: exit status 1")
			case 4:
				return "@4\t132\t41\nwindow-size latest\non\n", nil
			default:
				return "", nil
			}
		},
	}
	_, _, release, err := factory.pinWindow(Identity{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 5 {
		t.Fatalf("tmux calls = %d, want inherited capture/conflict/rollback and local capture/validation", len(calls))
	}
	if firstPin := strings.Join(calls[1], " "); !strings.Contains(
		firstPin, "set-option -w -o -t @4 window-size manual",
	) {
		t.Fatalf("inherited pin is not atomic set-if-unset: %q", firstPin)
	}
	if rollback := strings.Join(calls[2], " "); !strings.Contains(
		rollback, "set-option -w -u -t @4 window-size",
	) {
		t.Fatalf("inherited pin conflict was not rolled back safely: %q", rollback)
	}
	if localValidation := strings.Join(calls[4], " "); strings.Contains(
		localValidation, "set-option",
	) || strings.Contains(localValidation, "show-options") {
		t.Fatalf("local transition was overwritten by pin command %q", localValidation)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 5 {
		t.Fatalf("local window-size release issued a stale restore: calls=%d", len(calls))
	}
}

func TestPinWindowRollsBackPartialInheritedPinFailure(t *testing.T) {
	var calls [][]string
	factory := PTYViewerFactory{
		run: func(args []string) (string, error) {
			calls = append(calls, append([]string(nil), args...))
			switch len(calls) {
			case 1:
				return "@4\t132\t41\nwindow-size* latest\non\n", nil
			case 2:
				return "", errors.New("install mirror window hook: exit status 1")
			default:
				return "", nil
			}
		},
	}
	_, _, _, err := factory.pinWindow(Identity{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef",
	})
	if err == nil || !strings.Contains(err.Error(), "pin mirrored tmux window size") {
		t.Fatalf("partial pin error = %v", err)
	}
	if len(calls) != 3 {
		t.Fatalf("tmux calls = %d, want capture, pin, rollback", len(calls))
	}
	rollback := strings.Join(calls[2], " ")
	for _, want := range []string{
		"set-hook -w -u -t @4",
		"set-option -w -u -t @4 window-size",
		"@wrap_mirror_pin_owner",
	} {
		if !strings.Contains(rollback, want) {
			t.Fatalf("partial pin rollback %q missing %q", rollback, want)
		}
	}
}

func TestPinWindowLocalToInheritedTransitionNeverMutatesOrRestores(t *testing.T) {
	var calls [][]string
	factory := PTYViewerFactory{
		run: func(args []string) (string, error) {
			calls = append(calls, append([]string(nil), args...))
			if len(calls) == 1 {
				return "@4\t132\t41\nwindow-size latest\non\n", nil
			}
			return "", nil
		},
	}
	_, _, release, err := factory.pinWindow(Identity{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("tmux calls = %d, want capture and non-mutating validation", len(calls))
	}
	validation := strings.Join(calls[1], " ")
	if strings.Contains(validation, "set-option") || strings.Contains(validation, "show-options") ||
		strings.Contains(validation, "tmux") {
		t.Fatalf("local pin can overwrite a same-valued inherited transition: %q", validation)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("local pin release restored stale provenance: calls=%d", len(calls))
	}
}

func TestPinWindowLocalValidationFailureNeverRestoresStaleMode(t *testing.T) {
	for _, test := range []struct {
		name   string
		result string
		err    error
	}{
		{name: "command error", err: errors.New("host changed local option")},
		{name: "unexpected output", result: "unexpected validation output"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls [][]string
			factory := PTYViewerFactory{
				run: func(args []string) (string, error) {
					calls = append(calls, append([]string(nil), args...))
					if len(calls) == 1 {
						return "@4\t132\t41\nwindow-size latest\non\n", nil
					}
					return test.result, test.err
				},
			}
			_, _, _, err := factory.pinWindow(Identity{
				ID: "$7", Generation: "0123456789abcdef0123456789abcdef",
			})
			if err == nil {
				t.Fatal("local validation failure was accepted")
			}
			if len(calls) != 2 {
				t.Fatalf("local validation failure issued stale restore: calls=%d", len(calls))
			}
		})
	}
}

func TestInheritedWindowPinPreservesHostOverrideBeforeRelease(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf(
		"wrap-mirror-pin-release-%d-%d",
		os.Getpid(),
		viewerTestCounter.Add(1),
	)
	server := tmux.NewServer(socket)
	server.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = server.Run("kill-server") })
	if err := server.NewSession("viewer", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := server.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	sessions, err := server.Sessions()
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions = %+v, %v", sessions, err)
	}
	identity := Identity{ID: sessions[0].ID, Generation: generation}
	factory := PTYViewerFactory{SessionSocket: socket, TmuxPath: tmuxPath}
	windowID, err := server.Run("display-message", "-p", "-t", identity.ID, "#{window_id}")
	if err != nil {
		t.Fatal(err)
	}
	windowID = strings.TrimSpace(windowID)
	for _, hostValue := range []string{"latest", "manual"} {
		if _, err := server.Run("set-option", "-wu", "-t", windowID, "window-size"); err != nil {
			t.Fatal(err)
		}
		prepared, err := factory.Prepare(
			t.Context(), identity, func([]byte) error { return nil },
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := server.Run(
			"set-option", "-w", "-t", windowID, "window-size", hostValue,
		); err != nil {
			t.Fatal(err)
		}
		if err := prepared.Close(); err != nil {
			t.Fatal(err)
		}
		localValue, err := server.Run("show-options", "-wqv", "-t", windowID, "window-size")
		if err != nil || strings.TrimSpace(localValue) != hostValue {
			t.Fatalf(
				"release overwrote host window-size %q: value=%q err=%v",
				hostValue, localValue, err,
			)
		}
		owner, err := server.Run(
			"show-options", "-wqv", "-t", windowID, "@wrap_mirror_pin_owner",
		)
		if err != nil || strings.TrimSpace(owner) != "" {
			t.Fatalf("release left pin owner %q: %v", owner, err)
		}
		hooks, err := server.Run("show-hooks", "-w", "-t", windowID)
		if err != nil || strings.Contains(hooks, "@wrap_mirror_pin_owner") {
			t.Fatalf("release left pin ownership hook: hooks=%q err=%v", hooks, err)
		}
	}

	if _, err := server.Run("set-option", "-wu", "-t", windowID, "window-size"); err != nil {
		t.Fatal(err)
	}
	prepared, err := factory.Prepare(
		t.Context(), identity, func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run(
		"set-option", "-w", "-t", windowID, "pane-border-status", "top",
	); err != nil {
		t.Fatal(err)
	}
	if err := prepared.Close(); err != nil {
		t.Fatal(err)
	}
	localValue, err := server.Run("show-options", "-wqv", "-t", windowID, "window-size")
	if err != nil || strings.TrimSpace(localValue) != "" {
		t.Fatalf("unrelated host option stranded wrap pin: value=%q err=%v", localValue, err)
	}
	paneBorder, err := server.Run(
		"show-options", "-wqv", "-t", windowID, "pane-border-status",
	)
	if err != nil || strings.TrimSpace(paneBorder) != "top" {
		t.Fatalf("release disturbed unrelated host option: value=%q err=%v", paneBorder, err)
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
	hostCommand := exec.Command(tmuxPath, "-L", socket, "attach-session", "-t", identity.ID)
	hostCommand.Env = cleanViewerEnvironment(os.Environ())
	hostPTY, err := pty.StartWithSize(hostCommand, &pty.Winsize{Cols: 184, Rows: 80})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stopUnverifiedViewer(hostCommand, hostPTY); err != nil {
			t.Errorf("stop host tmux client: %v", err)
		}
	})
	wantHostSize := "184x79"
	deadline := time.Now().Add(3 * time.Second)
	for {
		got, sizeErr := server.Run(
			"display-message", "-p", "-t", identity.ID, "#{window_width}x#{window_height}",
		)
		if sizeErr == nil && strings.TrimSpace(got) == wantHostSize {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("host tmux client size = %q, %v; want %s", got, sizeErr, wantHostSize)
		}
		time.Sleep(10 * time.Millisecond)
	}
	sizeBefore, err := server.Run(
		"display-message", "-p", "-t", identity.ID, "#{window_width}x#{window_height}",
	)
	if err != nil {
		t.Fatal(err)
	}
	var outputMu sync.Mutex
	var output strings.Builder
	outputReady := make(chan struct{}, 1)
	var diagnosticEvents []DiagnosticRecord
	factory := PTYViewerFactory{
		SessionSocket: socket,
		TmuxPath:      tmuxPath,
		Record: func(record DiagnosticRecord) {
			diagnosticEvents = append(diagnosticEvents, record)
		},
	}
	prepared, err := factory.Prepare(t.Context(), identity, func(chunk []byte) error {
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
	status, err := server.Run("show-options", "-v", "-A", "-t", identity.ID, "status")
	if err != nil {
		t.Fatal(err)
	}
	statusRows, err := parseStatusRows(status)
	if err != nil {
		t.Fatal(err)
	}
	var windowColumns, windowRows uint16
	if _, err := fmt.Sscanf(strings.TrimSpace(sizeBefore), "%dx%d", &windowColumns, &windowRows); err != nil {
		t.Fatal(err)
	}
	wantGeometry := fmt.Sprintf("%dx%d", windowColumns, windowRows+statusRows)
	gotGeometry := fmt.Sprintf("%dx%d", prepared.Geometry().Columns, prepared.Geometry().Rows)
	if gotGeometry != wantGeometry {
		t.Fatalf("prepared viewer geometry = %s, want %s", gotGeometry, wantGeometry)
	}
	viewer, err := prepared.Start()
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnosticEvents) < 2 || diagnosticEvents[0].Event != "geometry_preparing" ||
		diagnosticEvents[len(diagnosticEvents)-1].Event != "geometry_verified" {
		t.Fatalf("viewer geometry diagnostics = %+v", diagnosticEvents)
	}
	ptyViewer, ok := viewer.(*ptyViewer)
	if !ok {
		t.Fatalf("viewer = %T, want *ptyViewer", viewer)
	}
	clientResult, err := server.Run(
		"list-clients", "-F",
		"#{client_name}\t#{client_pid}\t#{client_width}\t#{client_height}\t#{client_flags}\t#{window_id}\t#{window_width}\t#{window_height}\t#{window_bigger}\t#{status}",
	)
	if err != nil {
		t.Fatal(err)
	}
	clientGeometry, err := parseViewerClientGeometry(clientResult, ptyViewer.command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	windowID, err := server.Run("display-message", "-p", "-t", identity.ID, "#{window_id}")
	if err != nil {
		t.Fatal(err)
	}
	if !viewerGeometryMatches(prepared.Geometry(), strings.TrimSpace(windowID), clientGeometry) {
		t.Fatalf("first attached client geometry = %+v, captured = %+v", clientGeometry, prepared.Geometry())
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
	secondPrepared, err := factory.Prepare(
		t.Context(),
		identity,
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	secondViewer, err := secondPrepared.Start()
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

	endingPrepared, err := factory.Prepare(
		t.Context(),
		identity,
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	endingViewer, err := endingPrepared.Start()
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
	_, err = factory.Prepare(
		context.Background(),
		identity,
		func([]byte) error { return nil },
	)
	if !errors.Is(err, tmux.ErrServerGenerationChanged) {
		t.Fatalf("generation-mismatched viewer = %v, want ErrServerGenerationChanged", err)
	}
}

func TestParseWindowPinPreservesLocalAndInheritedModes(t *testing.T) {
	for _, test := range []struct {
		name       string
		result     string
		windowID   string
		columns    uint16
		rows       uint16
		statusRows uint16
		status     string
		mode       string
		inherited  bool
	}{
		{"local", "@4\t132\t41\nwindow-size largest\non\n", "@4", 132, 42, 1, "on", "largest", false},
		{"inherited", "@9\t80\t24\nwindow-size* latest\noff\n", "@9", 80, 24, 0, "off", "latest", true},
		{"multi-line status", "@10\t90\t30\nwindow-size* latest\n3\n", "@10", 90, 33, 3, "3", "latest", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			windowID, geometry, mode, inherited, err := parseWindowPin(test.result)
			if err != nil {
				t.Fatal(err)
			}
			if geometry != (ViewerGeometry{
				Columns: test.columns, Rows: test.rows, statusRows: test.statusRows,
				statusValue: test.status,
			}) {
				t.Fatalf("parseWindowPin geometry = %+v", geometry)
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
		"$4\t80\t24\nwindow-size latest\non\n",
		"@4\twide\t24\nwindow-size latest\non\n",
		"@4\t80\t24\nwindow-size unsafe\non\n",
		"@4\t80\t24\nunknown latest\non\n",
		"@4\t80\t24\nwindow-size latest\nunsafe\n",
	} {
		if _, _, _, _, err := parseWindowPin(invalid); err == nil {
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
	firstPrepared, err := factory.Prepare(
		t.Context(),
		identities[0],
		func([]byte) error { return nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := firstPrepared.Start()
	if err != nil {
		t.Fatal(err)
	}
	secondPrepared, err := factory.Prepare(
		t.Context(),
		identities[1],
		func([]byte) error { return nil },
	)
	if err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	second, err := secondPrepared.Start()
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
