package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var tmuxTestCounter atomic.Uint64

type fakeRunner struct {
	calls         [][]string
	out           string
	err           error
	outByContains map[string]string
}

func (f *fakeRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for needle, out := range f.outByContains {
		if strings.Contains(joined, needle) {
			return out, nil
		}
	}
	return f.out, f.err
}

func (f *fakeRunner) last() string {
	if len(f.calls) == 0 {
		return ""
	}
	return strings.Join(f.calls[len(f.calls)-1], " ")
}

func TestServerCommands(t *testing.T) {
	f := &fakeRunner{}
	s := &Server{Socket: "wrap", R: f}

	if err := s.NewSession("p/e", "/tmp/x", "myagent"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap new-session -d -s p/e -c /tmp/x myagent" {
		t.Errorf("NewSession: %q", got)
	}
	if err := s.NewSession("p/e", "/tmp/x", ""); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap new-session -d -s p/e -c /tmp/x" {
		t.Errorf("NewSession empty cmd: %q", got)
	}
	f.out = "$7"
	if id, err := s.NewSessionID("p/e", "/tmp/x", "myagent"); err != nil || id != "$7" {
		t.Fatalf("NewSessionID = %q, %v", id, err)
	}
	if got := f.last(); got != "-L wrap new-session -d -P -F #{session_id} -s p/e -c /tmp/x myagent" {
		t.Errorf("NewSessionID: %q", got)
	}
	f.out = "$8"
	const expectedGeneration = "0123456789abcdef0123456789abcdef"
	id, generation, err := s.NewSessionIdentity("p/e", "/tmp/x", "myagent", expectedGeneration)
	if err != nil || id != "$8" || generation != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("NewSessionIdentity = %q, %q, %v", id, generation, err)
	}
	if got := f.last(); !strings.Contains(got, "if-shell -F #{==:#{@wrap_server_generation},"+expectedGeneration+"}") ||
		!strings.Contains(got, "new-session -d -P -F") {
		t.Errorf("NewSessionIdentity: %q", got)
	}
	f.out = ""
	if err := s.KillSession("p/e"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap kill-session -t =p/e" {
		t.Errorf("KillSession: %q", got)
	}
	if err := s.KillSessionID("$7"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap kill-session -t $7" {
		t.Errorf("KillSessionID: %q", got)
	}
	if err := s.RenameSession("p/e", "p/e2"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap rename-session -t =p/e p/e2" {
		t.Errorf("RenameSession: %q", got)
	}
	if err := s.RenameSessionID("$7", "p/e3"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap rename-session -t $7 p/e3" {
		t.Errorf("RenameSessionID: %q", got)
	}
	if err := s.SwitchClient("/dev/ttys004", "p/e"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap switch-client -c /dev/ttys004 -t =p/e" {
		t.Errorf("SwitchClient: %q", got)
	}
	if err := s.SwitchClient("/dev/ttys004", "$7"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap switch-client -c /dev/ttys004 -t $7" {
		t.Errorf("SwitchClient stable ID: %q", got)
	}
	if err := s.SwitchClient("/dev/ttys004", "$workspace·term·1"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap switch-client -c /dev/ttys004 -t =$workspace·term·1" {
		t.Errorf("SwitchClient dollar-prefixed name: %q", got)
	}
	if err := s.SelectPane("wrap:0.1"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap select-pane -t wrap:0.1" {
		t.Errorf("SelectPane: %q", got)
	}
	if err := s.Set("detach-on-destroy", "off"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap set-option -g detach-on-destroy off" {
		t.Errorf("Set: %q", got)
	}
}

func TestPaneHeightAndResizeUseStablePaneID(t *testing.T) {
	f := &fakeRunner{out: "31"}
	s := &Server{Socket: "wrap-ui", R: f}
	height, err := s.PaneHeight("%3")
	if err != nil {
		t.Fatal(err)
	}
	if height != 31 {
		t.Fatalf("pane height = %d, want 31", height)
	}
	if got := f.last(); got != "-L wrap-ui display-message -p -t %3 #{pane_height}" {
		t.Fatalf("pane height command = %q", got)
	}
	if err := s.ResizePaneHeight("%3", 42); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap-ui resize-pane -t %3 -y 42" {
		t.Fatalf("resize pane command = %q", got)
	}
}

func TestPaneHeightAndResizeRejectInvalidArguments(t *testing.T) {
	for _, pane := range []string{"", "3", "%x", "%3;kill-server"} {
		s := &Server{Socket: "wrap-ui", R: &fakeRunner{out: "31"}}
		if _, err := s.PaneHeight(pane); err == nil {
			t.Errorf("PaneHeight accepted pane %q", pane)
		}
		if err := s.ResizePaneHeight(pane, 42); err == nil {
			t.Errorf("ResizePaneHeight accepted pane %q", pane)
		}
	}
	for _, height := range []int{0, -1, 301} {
		s := &Server{Socket: "wrap-ui", R: &fakeRunner{}}
		if err := s.ResizePaneHeight("%3", height); err == nil {
			t.Errorf("ResizePaneHeight accepted height %d", height)
		}
	}
	for _, output := range []string{"", "zero", "0", "301"} {
		s := &Server{Socket: "wrap-ui", R: &fakeRunner{out: output}}
		if _, err := s.PaneHeight("%3"); err == nil {
			t.Errorf("PaneHeight accepted output %q", output)
		}
	}
}

func TestEnsureServerGeneration(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	f := &fakeRunner{outByContains: map[string]string{
		"show-options -gvq @wrap_server_generation": generation,
	}}
	s := &Server{Socket: "wrap", R: f}
	got, err := s.EnsureServerGeneration(generation)
	if err != nil {
		t.Fatal(err)
	}
	if got != generation {
		t.Errorf("generation = %q, want %q", got, generation)
	}
	if len(f.calls) != 2 || !strings.Contains(strings.Join(f.calls[0], " "), "if-shell -F #{==:#{@wrap_server_generation},}") {
		t.Fatalf("generation initialization calls = %v", f.calls)
	}
}

func TestKillSessionIDIfGeneration(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	f := &fakeRunner{}
	s := &Server{Socket: "wrap", R: f}
	if err := s.KillSessionIDIfGeneration("$7", generation); err != nil {
		t.Fatal(err)
	}
	got := f.last()
	if !strings.Contains(got, "if-shell -F #{==:#{@wrap_server_generation},"+generation+"}") ||
		!strings.Contains(got, "kill-session -t $7") {
		t.Errorf("guarded kill command = %q", got)
	}

	f.out = "wrap-server-generation-mismatch"
	if err := s.KillSessionIDIfGeneration("$7", generation); !errors.Is(err, ErrServerGenerationChanged) {
		t.Fatalf("generation mismatch = %v, want ErrServerGenerationChanged", err)
	}
}

func TestKillSessionIDIfGenerationAndNameAndKind(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	f := &fakeRunner{}
	s := &Server{Socket: "wrap", R: f}
	if err := s.KillSessionIDIfGenerationAndNameAndKind(
		"$7", generation, "vb·term·1", SessionKindScratch,
	); err != nil {
		t.Fatal(err)
	}
	got := f.last()
	if !strings.Contains(got, "#{==:#{@wrap_server_generation},"+generation+"}") ||
		!strings.Contains(got, "#{==:#{session_name},vb·term·1}") ||
		!strings.Contains(got, "#{==:#{@wrap_session_kind},scratch}") ||
		!strings.Contains(got, "kill-session -t $7") {
		t.Fatalf("identity-guarded kill command = %q", got)
	}

	f.out = "wrap-session-identity-mismatch"
	err := s.KillSessionIDIfGenerationAndNameAndKind(
		"$7", generation, "vb·term·1", SessionKindScratch,
	)
	if !errors.Is(err, ErrSessionIdentityChanged) {
		t.Fatalf("identity mismatch = %v, want ErrSessionIdentityChanged", err)
	}
}

func TestRenameSessionIDIfGenerationAndNameAndKind(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	f := &fakeRunner{}
	s := &Server{Socket: "wrap", R: f}
	if err := s.RenameSessionIDIfGenerationAndNameAndKind(
		"$7", generation, "vb·term·1", SessionKindScratch, "vb·term·logs",
	); err != nil {
		t.Fatal(err)
	}
	got := f.last()
	if !strings.Contains(got, "#{==:#{@wrap_server_generation},"+generation+"}") ||
		!strings.Contains(got, "#{==:#{session_name},vb·term·1}") ||
		!strings.Contains(got, "#{==:#{@wrap_session_kind},scratch}") ||
		!strings.Contains(got, "rename-session -t $7 vb·term·logs") {
		t.Fatalf("identity-guarded rename command = %q", got)
	}

	f.out = sessionIdentityMismatchMessage
	err := s.RenameSessionIDIfGenerationAndNameAndKind(
		"$7", generation, "vb·term·1", SessionKindScratch, "vb·term·logs",
	)
	if !errors.Is(err, ErrSessionIdentityChanged) {
		t.Fatalf("identity mismatch = %v, want ErrSessionIdentityChanged", err)
	}
}

func TestSetUnmarkedSessionKindIfGenerationAndName(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	f := &fakeRunner{}
	s := &Server{Socket: "wrap", R: f}
	if err := s.SetUnmarkedSessionKindIfGenerationAndName(
		"$7", generation, "vb·term·1", SessionKindScratch,
	); err != nil {
		t.Fatal(err)
	}
	got := f.last()
	for _, want := range []string{
		"#{==:#{@wrap_server_generation}," + generation + "}",
		"#{==:#{session_name},vb·term·1}",
		"#{==:#{@wrap_session_kind},}",
		"#{==:#{@wrap_entry_name},}",
		"#{==:#{@wrap_entry_path},}",
		"set-option -t $7 @wrap_session_kind scratch",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("legacy-kind guard missing %q in %q", want, got)
		}
	}

	f.out = sessionIdentityMismatchMessage
	err := s.SetUnmarkedSessionKindIfGenerationAndName(
		"$7", generation, "vb·term·1", SessionKindScratch,
	)
	if !errors.Is(err, ErrSessionIdentityChanged) {
		t.Fatalf("identity mismatch = %v, want ErrSessionIdentityChanged", err)
	}
}

func TestSetSessionKindIfGenerationAndNameAndCurrentKind(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	f := &fakeRunner{}
	s := &Server{Socket: "wrap", R: f}
	if err := s.SetSessionKindIfGenerationAndNameAndCurrentKind(
		"$7", generation, "vb/api_server", SessionKindScratch, SessionKindEntry,
	); err != nil {
		t.Fatal(err)
	}
	got := f.last()
	for _, want := range []string{
		"#{==:#{@wrap_server_generation}," + generation + "}",
		"#{==:#{session_name},vb/api_server}",
		"#{==:#{@wrap_session_kind},scratch}",
		"set-option -t $7 @wrap_session_kind entry",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("kind transition guard missing %q in %q", want, got)
		}
	}

	f.out = sessionIdentityMismatchMessage
	err := s.SetSessionKindIfGenerationAndNameAndCurrentKind(
		"$7", generation, "vb/api_server", "", SessionKindEntry,
	)
	if !errors.Is(err, ErrSessionIdentityChanged) {
		t.Fatalf("identity mismatch = %v, want ErrSessionIdentityChanged", err)
	}
}

func TestGenerationGuardedSessionMutations(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	f := &fakeRunner{}
	s := &Server{Socket: "wrap", R: f}
	optionValue := EncodeEntryName("vb/$USER's repo")
	if err := s.SetSessionOptionIfGeneration("$7", generation, EntryNameOption, optionValue); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); !strings.Contains(got, "set-option -t $7 "+EntryNameOption+" "+optionValue) {
		t.Errorf("guarded set-option command = %q", got)
	}
	if err := s.RenameSessionIDIfGeneration("$7", generation, "vb/repo"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); !strings.Contains(got, "rename-session -t $7 vb/repo") {
		t.Errorf("guarded rename command = %q", got)
	}
	if err := s.SwitchClientIfGeneration("/dev/ttys002", "$7", generation); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); !strings.Contains(got, "switch-client -c /dev/ttys002 -t $7") {
		t.Errorf("guarded switch command = %q", got)
	}
	f.out = "wrap-server-generation-mismatch"
	if err := s.SetSessionOptionIfGeneration("$7", generation, "@wrap_entry_name", "vb/repo"); !errors.Is(err, ErrServerGenerationChanged) {
		t.Fatalf("guarded set mismatch = %v", err)
	}
	if err := s.RenameSessionIDIfGeneration("$7", generation, "vb/repo"); !errors.Is(err, ErrServerGenerationChanged) {
		t.Fatalf("guarded rename mismatch = %v", err)
	}
	if err := s.SwitchClientIfGeneration("/dev/ttys002", "$7", generation); !errors.Is(err, ErrServerGenerationChanged) {
		t.Fatalf("guarded switch mismatch = %v", err)
	}
}

func TestTmuxCommandQuotesParserMetacharacters(t *testing.T) {
	got := tmuxCommand("set-option", "-t", "$7", "@wrap_test", "~project;$USER's")
	if !strings.Contains(got, "set-option -t $7 @wrap_test") {
		t.Fatalf("static arguments or session id were unexpectedly rewritten: %q", got)
	}
	if !strings.Contains(got, "'~project;$USER'\\''s'") {
		t.Fatalf("parser metacharacters were not quoted: %q", got)
	}
}

func TestAttachSessionIfGenerationArgs(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	args, err := AttachSessionIfGenerationArgs("$7", generation)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "if-shell -F #{==:#{@wrap_server_generation},"+generation+"}") ||
		!strings.Contains(got, "attach-session -t $7") ||
		!strings.Contains(got, generationMismatchMessage) {
		t.Fatalf("generation-guarded attach args = %q", got)
	}
}

func TestAttachSessionIgnoringSizeIfGenerationArgs(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	args, err := AttachSessionIgnoringSizeIfGenerationArgs("$7", generation)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	if !strings.Contains(got, "if-shell -F #{==:#{@wrap_server_generation},"+generation+"}") ||
		!strings.Contains(got, "attach-session -f ignore-size -t $7") ||
		!strings.Contains(got, generationMismatchMessage) {
		t.Fatalf("generation-guarded ignore-size attach args = %q", got)
	}
}

func TestWindowSizePinAndRestoreArgsAreGenerationGuarded(t *testing.T) {
	if IsMissingTargetError(nil) {
		t.Fatal("nil error classified as a missing tmux target")
	}
	const generation = "0123456789abcdef0123456789abcdef"
	const owner = "00112233445566778899aabbccddeeff"
	const hookIndex = 424242
	capture, err := CaptureWindowSizeIfGenerationArgs("$7", generation)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(capture, " ")
	for _, want := range []string{
		"if-shell -F #{==:#{@wrap_server_generation}," + generation + "}",
		"display-message -p -t $7",
		"#{window_id}",
		"#{window_width}",
		"#{window_height}",
		"show-options -w -A -t $7 window-size",
		"show-options -v -A -t $7 status",
		generationMismatchMessage,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("window-size capture args %q missing %q", got, want)
		}
	}
	if strings.Contains(got, "set-option") {
		t.Fatalf("window-size capture mutates tmux state: %q", got)
	}
	pin, err := PinWindowSizeIfGenerationArgs(
		"$7", generation, "@4", 132, 41, "latest", true, "on", owner, hookIndex,
	)
	if err != nil {
		t.Fatal(err)
	}
	got = strings.Join(pin, " ")
	for _, want := range []string{
		"#{==:#{@wrap_server_generation}," + generation + "}",
		"#{==:#{window_id},@4}",
		"#{==:#{window_width},132}",
		"#{==:#{window_height},41}",
		"#{==:#{window-size},latest}",
		"#{==:#{status},on}",
		"set-option -w -o -t @4 window-size manual",
		"resize-window -t @4 -x 132 -y 41",
		"set-option -w -t @4 " + windowPinOwnerOption + " " + owner,
		"set-hook -w -t @4",
		"after-set-option[424242]",
		"#{==:#{hook_argument_0},window-size}",
		"after-resize-window[424242]",
		windowPinMismatchMessage,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("window-size pin args %q missing %q", got, want)
		}
	}
	claimAt := strings.Index(got, "set-option -w -o -t @4 window-size manual")
	markAt := strings.Index(got, "set-option -w -t @4 "+windowPinOwnerOption+" "+owner)
	resizeAt := strings.Index(got, "resize-window -t @4 -x 132 -y 41")
	hookAt := strings.Index(got, "set-hook -w -t @4")
	if claimAt < 0 || markAt <= claimAt || resizeAt <= markAt || hookAt <= resizeAt {
		t.Fatalf("window-size pin does not restore captured geometry before arming hooks: %q", got)
	}
	if _, err := PinWindowSizeIfGenerationArgs(
		"$7", generation, "@4", 132, 41, "unsafe", true, "on", owner, hookIndex,
	); err == nil {
		t.Fatal("accepted invalid captured window-size mode")
	}
	localPin, err := PinWindowSizeIfGenerationArgs(
		"$7", generation, "@4", 132, 41, "latest", false, "on", "", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(localPin, " "); strings.Contains(got, "set-option") ||
		strings.Contains(got, "show-options") || strings.Contains(got, "tmux") {
		t.Fatalf("local window-size pin mutates or shells out: %q", got)
	}
	if !IsWindowPinConflictError(errors.New("already set: window-size: exit status 1")) {
		t.Fatal("atomic inherited-window conflict was not classified for retry")
	}
	if IsWindowPinConflictError(errors.New("already set: status: exit status 1")) {
		t.Fatal("unrelated tmux option conflict was classified as a pin retry")
	}
	restore, err := RestoreWindowSizeIfGenerationArgs(generation, "@4", "latest", false, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	got = strings.Join(restore, " ")
	if !strings.Contains(got, "set-option -w -t @4 window-size latest") ||
		!strings.Contains(got, generationMismatchMessage) {
		t.Fatalf("window-size restore args = %q", got)
	}
	inherited, err := RestoreWindowSizeIfGenerationArgs(
		generation, "@4", "latest", true, owner, hookIndex,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(inherited, " "); !strings.Contains(
		got, "#{==:#{window-size},manual}",
	) || !strings.Contains(got, "#{==:#{"+windowPinOwnerOption+"},"+owner+"}") ||
		!strings.Contains(got, "set-hook -w -u -t @4") ||
		!strings.Contains(got, "after-set-option[424242]") ||
		!strings.Contains(got, "after-resize-window[424242]") ||
		!strings.Contains(got, "set-option -w -u -t @4 window-size") ||
		!strings.Contains(got, "#{==:#{"+windowPinOwnerOption+"},"+owner+"}") {
		t.Fatalf("inherited window-size restore is not pinned-state guarded: %q", got)
	}
	if _, err := RestoreWindowSizeIfGenerationArgs(
		generation, "@4", "unsafe", false, "", 0,
	); err == nil {
		t.Fatal("accepted invalid window-size mode")
	}
}

func TestWindowSizePinPreservesSameValuedProvenanceTransitions(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("wrap-pin-provenance-%d-%d", os.Getpid(), tmuxTestCounter.Add(1))
	server := NewServer(socket)
	server.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = server.Run("kill-server") })
	if err := server.NewSession("target", t.TempDir(), ""); err != nil {
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
	id := sessions[0].ID
	geometry, err := server.Run(
		"display-message", "-p", "-t", id,
		"#{window_id}\t#{window_width}\t#{window_height}\t#{status}",
	)
	if err != nil {
		t.Fatal(err)
	}
	var windowID, status string
	var columns, rows uint16
	if _, err := fmt.Sscanf(
		strings.TrimSpace(geometry), "%s\t%d\t%d\t%s", &windowID, &columns, &rows, &status,
	); err != nil {
		t.Fatalf("parse geometry %q: %v", geometry, err)
	}

	inheritedPin, err := PinWindowSizeIfGenerationArgs(
		id, generation, windowID, columns, rows, "latest", true, status,
		"00112233445566778899aabbccddeeff", 424242,
	)
	if err != nil {
		t.Fatal(err)
	}
	{
		if output, err := server.Run(inheritedPin...); err != nil || strings.TrimSpace(output) != "" {
			t.Fatalf("inherited pin = %q, %v", output, err)
		}
		if _, err := server.Run("set-option", "-w", "-t", windowID, "window-size", "manual"); err != nil {
			t.Fatal(err)
		}
		owner, err := server.Run(
			"show-options", "-wqv", "-t", windowID, windowPinOwnerOption,
		)
		if err != nil || strings.TrimSpace(owner) != "" {
			t.Fatalf("same-valued host window-size change retained owner marker %q: %v", owner, err)
		}
		restore, err := RestoreWindowSizeIfGenerationArgs(
			generation, windowID, "latest", true,
			"00112233445566778899aabbccddeeff", 424242,
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := server.Run(restore...); err != nil {
			t.Fatal(err)
		}
		localValue, err := server.Run("show-options", "-wqv", "-t", windowID, "window-size")
		if err != nil || strings.TrimSpace(localValue) != "manual" {
			t.Fatalf("same-valued host window-size change was restored away: value=%q err=%v", localValue, err)
		}
		if _, err := server.Run("set-option", "-wu", "-t", windowID, "window-size"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := server.Run("set-option", "-w", "-t", windowID, "window-size", "latest"); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run(inheritedPin...); !IsWindowPinConflictError(err) {
		t.Fatalf("inherited-to-local pin = %v, want atomic conflict", err)
	}
	localValue, err := server.Run("show-options", "-wqv", "-t", windowID, "window-size")
	if err != nil || strings.TrimSpace(localValue) != "latest" {
		t.Fatalf("inherited-to-local transition overwritten: value=%q err=%v", localValue, err)
	}
	owner, err := server.Run(
		"show-options", "-wqv", "-t", windowID, windowPinOwnerOption,
	)
	if err != nil || strings.TrimSpace(owner) != "" {
		t.Fatalf("failed inherited pin left owner marker %q: %v", owner, err)
	}
	hooks, err := server.Run("show-hooks", "-w", "-t", windowID)
	if err != nil || strings.Contains(hooks, windowPinOwnerOption) {
		t.Fatalf("failed inherited pin left ownership hook: hooks=%q err=%v", hooks, err)
	}

	localPin, err := PinWindowSizeIfGenerationArgs(
		id, generation, windowID, columns, rows, "latest", false, status, "", 0,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("set-option", "-wu", "-t", windowID, "window-size"); err != nil {
		t.Fatal(err)
	}
	if output, err := server.Run(localPin...); err != nil || strings.TrimSpace(output) != "" {
		t.Fatalf("local-to-inherited validation = %q, %v", output, err)
	}
	localValue, err = server.Run("show-options", "-wqv", "-t", windowID, "window-size")
	if err != nil || strings.TrimSpace(localValue) != "" {
		t.Fatalf("local-to-inherited transition recreated: value=%q err=%v", localValue, err)
	}
}

func TestAttachWindowIgnoringSizeGuardsCurrentWindow(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	args, err := AttachWindowIgnoringSizeIfGenerationArgs("$7", generation, "@4")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(args, " ")
	for _, want := range []string{
		"if-shell -F -t $7",
		"#{==:#{@wrap_server_generation}," + generation + "}",
		"#{==:#{window_id},@4}",
		"attach-session -f ignore-size -t $7",
		generationMismatchMessage,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("window-guarded attach args %q missing %q", got, want)
		}
	}
}

func TestAttachWindowGuardRefusesChangedCurrentWindow(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf(
		"wrap-attach-window-%d-%d",
		os.Getpid(),
		tmuxTestCounter.Add(1),
	)
	server := NewServer(socket)
	server.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = server.Run("kill-server") })
	if err := server.NewSession("target", t.TempDir(), ""); err != nil {
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
	windowID, err := server.Run("display-message", "-p", "-t", sessions[0].ID, "#{window_id}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("new-window", "-d", "-t", sessions[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Run("select-window", "-t", sessions[0].ID+":1"); err != nil {
		t.Fatal(err)
	}
	args, err := AttachWindowIgnoringSizeIfGenerationArgs(
		sessions[0].ID,
		generation,
		strings.TrimSpace(windowID),
	)
	if err != nil {
		t.Fatal(err)
	}
	output, err := server.Run(args...)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output) != generationMismatchMessage {
		t.Fatalf("changed-window attach output = %q", output)
	}
}

func TestServerGenerationGuardWithRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("wrap-generation-%d", os.Getpid())
	s := NewServer(socket)
	s.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = s.Run("kill-server") })
	if err := s.NewSession("target", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if got, err := s.EnsureServerGeneration(generation); err != nil || got != generation {
		t.Fatalf("EnsureServerGeneration = %q, %v", got, err)
	}
	infos, err := s.Sessions()
	if err != nil || len(infos) != 1 {
		t.Fatalf("Sessions = %+v, %v", infos, err)
	}
	if infos[0].Generation != generation || infos[0].ID == "" {
		t.Fatalf("session identity = %+v", infos[0])
	}
	const entryName = "value$USER"
	entryNameToken := EncodeEntryName(entryName)
	if err := s.SetSessionOptionIfGeneration(infos[0].ID, generation, EntryNameOption, entryNameToken); err != nil {
		t.Fatalf("matching generation set-option = %v", err)
	}
	if got, err := s.Run("display-message", "-p", "-t", infos[0].ID, "#{"+EntryNameOption+"}"); err != nil || got != entryNameToken {
		t.Fatalf("guarded option value = %q, %v", got, err)
	}
	infos, err = s.Sessions()
	if err != nil || len(infos) != 1 || infos[0].EntryName != entryName {
		t.Fatalf("decoded entry name = %+v, %v; want %q", infos, err, entryName)
	}
	const tildeValue = "~literal"
	if err := s.SetSessionOptionIfGeneration(infos[0].ID, generation, "@wrap_tilde", tildeValue); err != nil {
		t.Fatalf("leading-tilde set-option = %v", err)
	}
	if got, err := s.Run("display-message", "-p", "-t", infos[0].ID, "#{@wrap_tilde}"); err != nil || got != tildeValue {
		t.Fatalf("leading-tilde option value = %q, %v", got, err)
	}
	const renamed = "~renamed"
	if err := s.RenameSessionIDIfGeneration(infos[0].ID, generation, renamed); err != nil {
		t.Fatalf("matching generation rename = %v", err)
	}
	if alive, err := s.HasSession(renamed); err != nil || !alive {
		t.Fatalf("guarded rename missing: alive=%v err=%v", alive, err)
	}
	const otherGeneration = "fedcba9876543210fedcba9876543210"
	if err := s.KillSessionIDIfGeneration(infos[0].ID, otherGeneration); !errors.Is(err, ErrServerGenerationChanged) {
		t.Fatalf("mismatched generation kill = %v", err)
	}
	if alive, err := s.HasSession(renamed); err != nil || !alive {
		t.Fatalf("mismatched generation killed target: alive=%v err=%v", alive, err)
	}
	createdID, createdGeneration, err := s.NewSessionIdentity("created", t.TempDir(), "", generation)
	if err != nil || !isSessionID(createdID) || createdGeneration != generation {
		t.Fatalf("matching generation create = %q, %q, %v", createdID, createdGeneration, err)
	}
	if alive, err := s.HasSession("created"); err != nil || !alive {
		t.Fatalf("matching generation create missing: alive=%v err=%v", alive, err)
	}
	if err := s.KillSessionIDIfGeneration(infos[0].ID, generation); err != nil {
		t.Fatalf("matching generation kill = %v", err)
	}
}

func TestGenerationGuardedCreateRefusesRestartedServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("wrap-create-generation-%d", os.Getpid())
	s := NewServer(socket)
	s.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = s.Run("kill-server") })
	if err := s.NewSession("original", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const originalGeneration = "0123456789abcdef0123456789abcdef"
	if _, err := s.EnsureServerGeneration(originalGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run("kill-server"); err != nil {
		t.Fatal(err)
	}
	replacementRoot := t.TempDir()
	deadline := time.Now().Add(3 * time.Second)
	for {
		err := s.NewSession("unrelated", replacementRoot, "")
		if err == nil {
			break
		}
		if !strings.Contains(err.Error(), "server exited unexpectedly") ||
			time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, _, err := s.NewSessionIdentity("must-not-exist", t.TempDir(), "", originalGeneration); !errors.Is(err, ErrServerGenerationChanged) {
		t.Fatalf("guarded create after restart = %v, want ErrServerGenerationChanged", err)
	}
	if alive, err := s.HasSession("must-not-exist"); err != nil || alive {
		t.Fatalf("guarded create leaked a session after restart: alive=%v err=%v", alive, err)
	}
	if alive, err := s.HasSession("unrelated"); err != nil || !alive {
		t.Fatalf("guarded create disturbed replacement: alive=%v err=%v", alive, err)
	}
}

func TestRealTmuxSessionCurrentPathIfGenerationTracksLiveCD(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("wrap-current-path-%d", os.Getpid())
	s := NewServer(socket)
	s.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = s.Run("kill-server") })

	start := t.TempDir()
	next := filepath.Join(start, "café-路径")
	if err := os.Mkdir(next, 0o755); err != nil {
		t.Fatal(err)
	}
	canonicalStart, err := filepath.EvalSymlinks(start)
	if err != nil {
		t.Fatal(err)
	}
	canonicalNext, err := filepath.EvalSymlinks(next)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession("pwd", start, "/bin/sh"); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := s.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	infos, err := s.Sessions()
	if err != nil || len(infos) != 1 {
		t.Fatalf("Sessions = %+v, %v", infos, err)
	}
	id := infos[0].ID
	if got, err := s.SessionCurrentPathIfGeneration(id, generation); err != nil || got != canonicalStart {
		t.Fatalf("initial path = %q, %v; want %q", got, err, canonicalStart)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		command, err := s.Run("display-message", "-p", "-t", "pwd:0.0", "#{pane_current_command}")
		if err == nil && command != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("test shell did not become ready: command=%q err=%v", command, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	quoted := "'" + strings.ReplaceAll(next, "'", "'\\''") + "'"
	if _, err := s.Run("send-keys", "-t", "pwd:0.0", "cd -- "+quoted, "Enter"); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		got, err := s.SessionCurrentPathIfGeneration(id, generation)
		if err == nil && got == canonicalNext {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("path did not update to %q: got %q err=%v", canonicalNext, got, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	if _, err := s.Run("kill-server"); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession("replacement", start, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionCurrentPathIfGeneration(id, generation); !errors.Is(err, ErrServerGenerationChanged) {
		t.Fatalf("stale path identity after restart = %v, want ErrServerGenerationChanged", err)
	}
}

func TestRealTmuxNameGuardRefusesRenamedSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("wrap-name-guard-%d", os.Getpid())
	s := NewServer(socket)
	s.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = s.Run("kill-server") })
	if err := s.NewSession("vb·term·1", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := s.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	infos, err := s.Sessions()
	if err != nil || len(infos) != 1 {
		t.Fatalf("Sessions = %+v, %v", infos, err)
	}
	if err := s.SetSessionOptionIfGeneration(
		infos[0].ID, generation, SessionKindOption, SessionKindScratch,
	); err != nil {
		t.Fatal(err)
	}
	if err := s.RenameSessionIDIfGeneration(infos[0].ID, generation, "vb/repo"); err != nil {
		t.Fatal(err)
	}
	err = s.KillSessionIDIfGenerationAndNameAndKind(
		infos[0].ID, generation, "vb·term·1", SessionKindScratch,
	)
	if !errors.Is(err, ErrSessionIdentityChanged) {
		t.Fatalf("renamed-session kill = %v, want ErrSessionIdentityChanged", err)
	}
	if alive, err := s.HasSession("vb/repo"); err != nil || !alive {
		t.Fatalf("renamed protected session disturbed: alive=%v err=%v", alive, err)
	}
}

func TestRealTmuxKindGuardRefusesScratchLookingEntrySession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("wrap-kind-guard-%d", os.Getpid())
	s := NewServer(socket)
	s.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = s.Run("kill-server") })
	if err := s.NewSession("vb·term·renamed-entry", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := s.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	infos, err := s.Sessions()
	if err != nil || len(infos) != 1 {
		t.Fatalf("Sessions = %+v, %v", infos, err)
	}
	if err := s.SetSessionOptionIfGeneration(
		infos[0].ID, generation, SessionKindOption, SessionKindEntry,
	); err != nil {
		t.Fatal(err)
	}
	err = s.KillSessionIDIfGenerationAndNameAndKind(
		infos[0].ID, generation, "vb·term·renamed-entry", SessionKindScratch,
	)
	if !errors.Is(err, ErrSessionIdentityChanged) {
		t.Fatalf("entry-kind kill = %v, want ErrSessionIdentityChanged", err)
	}
	if alive, err := s.HasSession("vb·term·renamed-entry"); err != nil || !alive {
		t.Fatalf("entry-kind session disturbed: alive=%v err=%v", alive, err)
	}
}

func TestRealTmuxLegacyKindMigrationIsMarkerGuarded(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("wrap-kind-migration-%d", os.Getpid())
	s := NewServer(socket)
	s.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = s.Run("kill-server") })
	if err := s.NewSession("vb·term·1", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession("vb·term·renamed-entry", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := s.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	infos, err := s.Sessions()
	if err != nil || len(infos) != 2 {
		t.Fatalf("Sessions = %+v, %v", infos, err)
	}
	byName := map[string]SessionInfo{}
	for _, info := range infos {
		byName[info.Name] = info
	}
	legacy := byName["vb·term·1"]
	if err := s.SetUnmarkedSessionKindIfGenerationAndName(
		legacy.ID, generation, legacy.Name, SessionKindScratch,
	); err != nil {
		t.Fatal(err)
	}
	protected := byName["vb·term·renamed-entry"]
	if err := s.SetSessionOptionIfGeneration(
		protected.ID, generation, EntryNameOption, EncodeEntryName("vb/repo"),
	); err != nil {
		t.Fatal(err)
	}
	err = s.SetUnmarkedSessionKindIfGenerationAndName(
		protected.ID, generation, protected.Name, SessionKindScratch,
	)
	if !errors.Is(err, ErrSessionIdentityChanged) {
		t.Fatalf("protected legacy migration = %v, want ErrSessionIdentityChanged", err)
	}
	infos, err = s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	byName = map[string]SessionInfo{}
	for _, info := range infos {
		byName[info.Name] = info
	}
	if got := byName["vb·term·1"].Kind; got != SessionKindScratch {
		t.Fatalf("legacy scratch kind = %q, want %q", got, SessionKindScratch)
	}
	if got := byName["vb·term·renamed-entry"].Kind; got != "" {
		t.Fatalf("protected session kind = %q, want empty", got)
	}
}

func TestRealTmuxEntryKindTransitionRequiresCapturedCurrentKind(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("wrap-entry-kind-transition-%d", os.Getpid())
	s := NewServer(socket)
	s.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = s.Run("kill-server") })
	for _, name := range []string{"vb/api_server", "vb/other"} {
		if err := s.NewSession(name, t.TempDir(), ""); err != nil {
			t.Fatal(err)
		}
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := s.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	infos, err := s.Sessions()
	if err != nil || len(infos) != 2 {
		t.Fatalf("Sessions = %+v, %v", infos, err)
	}
	byName := map[string]SessionInfo{}
	for _, info := range infos {
		byName[info.Name] = info
	}
	if err := s.SetSessionOptionIfGeneration(
		byName["vb/other"].ID, generation, SessionKindOption, SessionKindScratch,
	); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSessionKindIfGenerationAndNameAndCurrentKind(
		byName["vb/api_server"].ID, generation, "vb/api_server", "", SessionKindEntry,
	); err != nil {
		t.Fatal(err)
	}
	err = s.SetSessionKindIfGenerationAndNameAndCurrentKind(
		byName["vb/other"].ID, generation, "vb/other", "", SessionKindEntry,
	)
	if !errors.Is(err, ErrSessionIdentityChanged) {
		t.Fatalf("conflicting kind transition = %v, want ErrSessionIdentityChanged", err)
	}
	infos, err = s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	byName = map[string]SessionInfo{}
	for _, info := range infos {
		byName[info.Name] = info
	}
	if got := byName["vb/api_server"].Kind; got != SessionKindEntry {
		t.Fatalf("unmarked entry kind = %q, want %q", got, SessionKindEntry)
	}
	if got := byName["vb/other"].Kind; got != SessionKindScratch {
		t.Fatalf("conflicting session kind = %q, want %q", got, SessionKindScratch)
	}
}

func TestGenerationGuardedAttachRefusesRestartedServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := fmt.Sprintf("wrap-attach-generation-%d", os.Getpid())
	s := NewServer(socket)
	s.ConfigFile = os.DevNull
	t.Cleanup(func() { _, _ = s.Run("kill-server") })
	if err := s.NewSession("original", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const originalGeneration = "0123456789abcdef0123456789abcdef"
	if _, err := s.EnsureServerGeneration(originalGeneration); err != nil {
		t.Fatal(err)
	}
	original, err := s.Sessions()
	if err != nil || len(original) != 1 {
		t.Fatalf("original Sessions = %+v, %v", original, err)
	}
	if _, err := s.Run("kill-server"); err != nil {
		t.Fatal(err)
	}
	if err := s.NewSession("unrelated", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const replacementGeneration = "fedcba9876543210fedcba9876543210"
	if _, err := s.EnsureServerGeneration(replacementGeneration); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.Sessions()
	if err != nil || len(replacement) != 1 {
		t.Fatalf("replacement Sessions = %+v, %v", replacement, err)
	}
	if replacement[0].ID != original[0].ID {
		t.Fatalf("restart did not reuse test id: original=%s replacement=%s", original[0].ID, replacement[0].ID)
	}
	args, err := AttachSessionIfGenerationArgs(original[0].ID, originalGeneration)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.Run(args...)
	if err != nil {
		t.Fatalf("mismatched attach handshake = %v", err)
	}
	if strings.TrimSpace(out) != generationMismatchMessage {
		t.Fatalf("mismatched attach output = %q, want %q", out, generationMismatchMessage)
	}
	if alive, err := s.HasSession("unrelated"); err != nil || !alive {
		t.Fatalf("mismatched attach disturbed replacement: alive=%v err=%v", alive, err)
	}
}

func TestGenerationGuardedAttachConnectsMatchingServer(t *testing.T) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	suffix := fmt.Sprintf("%d", os.Getpid())
	target := NewServer("wrap-attach-target-" + suffix)
	target.ConfigFile = os.DevNull
	host := NewServer("wrap-attach-host-" + suffix)
	host.ConfigFile = os.DevNull
	t.Cleanup(func() {
		_, _ = host.Run("kill-server")
		_, _ = target.Run("kill-server")
	})
	if err := target.NewSession("intended", t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	const generation = "0123456789abcdef0123456789abcdef"
	if _, err := target.EnsureServerGeneration(generation); err != nil {
		t.Fatal(err)
	}
	infos, err := target.Sessions()
	if err != nil || len(infos) != 1 {
		t.Fatalf("target Sessions = %+v, %v", infos, err)
	}
	attachArgs, err := AttachSessionIfGenerationArgs(infos[0].ID, generation)
	if err != nil {
		t.Fatal(err)
	}
	commandArgs := append([]string{bin, "-f", os.DevNull, "-L", target.Socket}, attachArgs...)
	command := "unset TMUX TMUX_PANE; exec " + shellJoin(commandArgs)
	if err := host.NewSession("host", t.TempDir(), command); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		out, err := target.Run("list-clients", "-F", "#{client_session}")
		if err == nil && strings.TrimSpace(out) == "intended" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	hostOutput, _ := host.Run("capture-pane", "-p", "-t", "=host")
	t.Fatalf("matching generation did not attach to intended session; host pane:\n%s", hostOutput)
}

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
	}
	return strings.Join(quoted, " ")
}

func TestPanesIncludesWrapRole(t *testing.T) {
	f := &fakeRunner{out: "%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'\tterminal"}
	s := &Server{Socket: "wrap-ui", R: f}
	panes, err := s.Panes("wrap-vb:0")
	if err != nil {
		t.Fatal(err)
	}
	if len(panes) != 1 || panes[0].Role != "terminal" {
		t.Fatalf("panes = %+v, want terminal role", panes)
	}
}

func TestPanesTypesMissingTargetButPreservesOtherFailures(t *testing.T) {
	s := &Server{Socket: "wrap-ui", R: &fakeRunner{err: errors.New("can't find window: 0")}}
	if _, err := s.Panes("wrap-vb:0"); !errors.Is(err, ErrMissingTarget) {
		t.Fatalf("missing window error = %v, want ErrMissingTarget", err)
	}

	s.R = &fakeRunner{err: errors.New("permission denied")}
	if _, err := s.Panes("wrap-vb:0"); err == nil || errors.Is(err, ErrMissingTarget) {
		t.Fatalf("transport error = %v, want unclassified failure", err)
	}
}

func TestEntryPathEncodingRoundTripsProtocolCharacters(t *testing.T) {
	path := "/tmp/line\nwith\ttabs/café"
	token := EncodeEntryPath(path)
	if strings.ContainsAny(token, "\t\n") {
		t.Fatalf("encoded path is not safe for tab/newline session protocol: %q", token)
	}
	got, err := DecodeEntryPath(token)
	if err != nil || got != path {
		t.Fatalf("DecodeEntryPath(%q) = %q, %v; want %q", token, got, err, path)
	}
}

func TestEntryNameEncodingRoundTripsTmuxParserCharacters(t *testing.T) {
	name := "workspace/$USER's repo"
	token := EncodeEntryName(name)
	if !strings.HasPrefix(token, entryNameEncodingPrefix) ||
		strings.ContainsAny(token, "$'\\\t\n") {
		t.Fatalf("encoded entry name is not tmux-parser safe: %q", token)
	}
	got, err := DecodeEntryName(token)
	if err != nil || got != name {
		t.Fatalf("DecodeEntryName(%q) = %q, %v; want %q", token, got, err, name)
	}
}

func TestDecodeEntryNameAcceptsLegacyPlainMarker(t *testing.T) {
	const legacy = "vb/api"
	got, err := DecodeEntryName(legacy)
	if err != nil || got != legacy {
		t.Fatalf("DecodeEntryName(%q) = %q, %v", legacy, got, err)
	}
}

func TestDecodeEntryNameRejectsMalformedEncodedMarker(t *testing.T) {
	if _, err := DecodeEntryName(entryNameEncodingPrefix + "%%%"); err == nil {
		t.Fatal("DecodeEntryName accepted malformed encoded marker")
	}
}

func TestSessionsIncludesEntryPathAndCurrentPathIsSeparate(t *testing.T) {
	token := EncodeEntryPath("/repos/api")
	f := &fakeRunner{outByContains: map[string]string{
		"list-sessions": strings.Join([]string{
			"vb/api", "1", "0", "0", "", "vb/api", token, "$7",
		}, "\t"),
		"display-message": "/repos/api/subdir",
	}}
	s := &Server{Socket: "wrap", R: f}
	infos, err := s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || infos[0].EntryPathToken != token {
		t.Fatalf("sessions = %+v", infos)
	}
	path, err := s.SessionCurrentPath("$7")
	if err != nil || path != "/repos/api/subdir" {
		t.Fatalf("SessionCurrentPath = %q, %v", path, err)
	}
}

func TestSessionsIncludesCreationTime(t *testing.T) {
	f := &fakeRunner{outByContains: map[string]string{
		"list-sessions": strings.Join([]string{
			"vb/api", "1", "0", "0", "", "vb/api", "", "$7",
			"0123456789abcdef0123456789abcdef", "1722171607", SessionKindEntry,
		}, "\t"),
	}}
	s := &Server{Socket: "wrap", R: f}
	infos, err := s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 {
		t.Fatalf("Sessions returned %d rows, want 1: %+v", len(infos), infos)
	}
	if got := infos[0].Created; got != 1722171607 {
		t.Fatalf("Created = %d, want 1722171607", got)
	}
	if got := infos[0].Kind; got != SessionKindEntry {
		t.Fatalf("Kind = %q, want %q", got, SessionKindEntry)
	}
}

func TestSessionsRejectsMalformedTimestamps(t *testing.T) {
	tests := []struct {
		name     string
		activity string
		created  string
		want     string
	}{
		{name: "activity", activity: "invalid", created: "1722171607", want: "activity"},
		{name: "creation", activity: "1", created: "invalid", want: "creation time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRunner{outByContains: map[string]string{
				"list-sessions": strings.Join([]string{
					"vb/api", tt.activity, "0", "0", "", "vb/api", "", "$7",
					"0123456789abcdef0123456789abcdef", tt.created, SessionKindEntry,
				}, "\t"),
			}}
			s := &Server{Socket: "wrap", R: f}
			_, err := s.Sessions()
			if err == nil || !strings.Contains(err.Error(), tt.want) ||
				!strings.Contains(err.Error(), "vb/api") {
				t.Fatalf("Sessions error = %v, want %q parse context for vb/api", err, tt.want)
			}
		})
	}
}

func TestSessionCurrentPathIfGenerationPreservesArbitraryPath(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	path := "/tmp/line\nwith\ttabs"
	f := &fakeRunner{out: path}
	s := &Server{Socket: "wrap", R: f}
	got, err := s.SessionCurrentPathIfGeneration("$7", generation)
	if err != nil {
		t.Fatal(err)
	}
	if got != path {
		t.Fatalf("SessionCurrentPathIfGeneration = %q, want %q", got, path)
	}
	command := f.last()
	if !strings.Contains(command, "if-shell -F #{==:#{@wrap_server_generation},"+generation+"}") ||
		!strings.Contains(command, "display-message -p -t $7") ||
		!strings.Contains(command, "#{pane_current_path}") {
		t.Fatalf("guarded current-path command = %q", command)
	}
}

func TestSessionCurrentPathIfGenerationRejectsInvalidIdentityBeforeTmux(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	tests := []struct {
		name, id, generation string
	}{
		{"bad id", "named-session", generation},
		{"bad generation", "$7", "not-a-generation"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeRunner{}
			s := &Server{Socket: "wrap", R: f}
			if _, err := s.SessionCurrentPathIfGeneration(tt.id, tt.generation); err == nil {
				t.Fatal("SessionCurrentPathIfGeneration = nil error, want invalid identity failure")
			}
			if len(f.calls) != 0 {
				t.Fatalf("invalid identity invoked tmux: %v", f.calls)
			}
		})
	}
}

func TestSessionCurrentPathIfGenerationClassifiesFailures(t *testing.T) {
	const generation = "0123456789abcdef0123456789abcdef"
	f := &fakeRunner{out: generationMismatchMessage}
	s := &Server{Socket: "wrap", R: f}
	if _, err := s.SessionCurrentPathIfGeneration("$7", generation); !errors.Is(err, ErrServerGenerationChanged) {
		t.Fatalf("path generation mismatch = %v, want ErrServerGenerationChanged", err)
	}

	f.out = ""
	f.err = errors.New("can't find session: 7")
	if _, err := s.SessionCurrentPathIfGeneration("$7", generation); !errors.Is(err, ErrMissingTarget) {
		t.Fatalf("missing path target = %v, want ErrMissingTarget", err)
	}

	f.err = errors.New("permission denied")
	if _, err := s.SessionCurrentPathIfGeneration("$7", generation); err == nil || errors.Is(err, ErrMissingTarget) {
		t.Fatalf("transport failure = %v, want unclassified error", err)
	}
}

func TestIsGenerationMismatchOutput(t *testing.T) {
	for _, output := range []string{
		"wrap-server-generation-mismatch",
		" wrap-server-generation-mismatch\n",
	} {
		if !IsGenerationMismatchOutput(output) {
			t.Fatalf("IsGenerationMismatchOutput(%q) = false", output)
		}
	}
	for _, output := range []string{
		"",
		"wrap-session-identity-mismatch",
		"prefix wrap-server-generation-mismatch",
	} {
		if IsGenerationMismatchOutput(output) {
			t.Fatalf("IsGenerationMismatchOutput(%q) = true", output)
		}
	}
}

func TestServerConfigFile(t *testing.T) {
	f := &fakeRunner{}
	s := &Server{Socket: "wrap-ui", ConfigFile: "/dev/null", R: f}
	if _, err := s.Run("set-option", "-g", "status", "off"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-f /dev/null -L wrap-ui set-option -g status off" {
		t.Errorf("ConfigFile not prefixed: %q", got)
	}
	// Without ConfigFile the user's tmux.conf applies (session server).
	s2 := &Server{Socket: "wrap", R: f}
	if _, err := s2.Run("has-session", "-t", "=x"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap has-session -t =x" {
		t.Errorf("unexpected prefix without ConfigFile: %q", got)
	}
}

func TestSessionsParsing(t *testing.T) {
	f := &fakeRunner{outByContains: map[string]string{
		"list-sessions":   "p/a\t1721700000\t0\t0\t\tp/a\t\t$1\tgen-a\np/b\t1721700100\t1\t1\t\t\t\t$2\tgen-b",
		"display-message": "/tmp/path",
	}}
	s := &Server{Socket: "wrap", R: f}
	infos, err := s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("infos = %+v", infos)
	}
	if infos[0].ID != "$1" || infos[0].Name != "p/a" || infos[0].Activity != 1721700000 || infos[0].Attached || infos[0].Bell || infos[0].EntryName != "p/a" {
		t.Errorf("infos[0] = %+v", infos[0])
	}
	if infos[0].Generation != "gen-a" || infos[1].Generation != "gen-b" {
		t.Errorf("session generations not parsed: %+v", infos)
	}
	if !infos[1].Attached || !infos[1].Bell {
		t.Errorf("infos[1] = %+v", infos[1])
	}
}

// The bell must be reported for an ATTACHED session, where tmux's own
// window_bell_flag is always 0 — that is the case wrap's terminal pane is
// permanently in, and why bells appeared not to work at all.
func TestSessionsBellFromWrapOptionWhenAttached(t *testing.T) {
	f := &fakeRunner{out: "onscreen\t1721700000\t1\t0\t1\nquiet\t1721700000\t1\t0\t"}
	s := &Server{Socket: "wrap", R: f}
	infos, err := s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("infos = %+v", infos)
	}
	if !infos[0].Bell {
		t.Errorf("attached session with %s=1 should report a bell: %+v", BellOption, infos[0])
	}
	if infos[1].Bell {
		t.Errorf("attached session with the option unset should not: %+v", infos[1])
	}
}

// A detached session still reports via tmux's own flag.
func TestSessionsBellFromTmuxFlagWhenDetached(t *testing.T) {
	f := &fakeRunner{out: "bg\t1721700000\t0\t1\t"}
	s := &Server{Socket: "wrap", R: f}
	infos, err := s.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 1 || !infos[0].Bell {
		t.Errorf("detached session with window_bell_flag=1 should report a bell: %+v", infos)
	}
}

func TestSetW(t *testing.T) {
	f := &fakeRunner{}
	s := &Server{Socket: "wrap", R: f}
	if err := s.SetW("monitor-bell", "on"); err != nil {
		t.Fatal(err)
	}
	if got := f.last(); got != "-L wrap set-option -wg monitor-bell on" {
		t.Errorf("SetW: %q", got)
	}
}

func TestSessionsNoServer(t *testing.T) {
	f := &fakeRunner{err: errors.New("tmux list-sessions: exit 1: no server running on /tmp/x")}
	s := &Server{Socket: "wrap", R: f}
	infos, err := s.Sessions()
	if !errors.Is(err, ErrNoServer) || infos != nil {
		t.Errorf("no-server should be typed: %v %v", infos, err)
	}
}

func TestSessionsPreservesUnrelatedMissingFileError(t *testing.T) {
	f := &fakeRunner{err: errors.New("load helper: No such file or directory")}
	s := &Server{Socket: "wrap", R: f}
	if _, err := s.Sessions(); err == nil || errors.Is(err, ErrNoServer) {
		t.Fatalf("unrelated missing-file error was classified as no-server: %v", err)
	}
}

func TestClientSession(t *testing.T) {
	f := &fakeRunner{out: "p/e"}
	s := &Server{Socket: "wrap", R: f}
	got, err := s.ClientSession("/dev/ttys002")
	if err != nil {
		t.Fatal(err)
	}
	if got != "p/e" {
		t.Errorf("ClientSession = %q, want p/e", got)
	}
	if last := f.last(); last != "-L wrap display-message -p -c /dev/ttys002 #{client_session}" {
		t.Errorf("ClientSession command: %q", last)
	}
}

func TestClientSessionIdentity(t *testing.T) {
	f := &fakeRunner{out: "renamed\t$7"}
	s := &Server{Socket: "wrap", R: f}
	name, id, err := s.ClientSessionIdentity("/dev/ttys002")
	if err != nil {
		t.Fatal(err)
	}
	if name != "renamed" || id != "$7" {
		t.Errorf("ClientSessionIdentity = %q, %q, want renamed, $7", name, id)
	}
	if last := f.last(); last != "-L wrap display-message -p -c /dev/ttys002 #{client_session}\t#{session_id}" {
		t.Errorf("ClientSessionIdentity command: %q", last)
	}
}

func TestClientSessionIdentityRejectsIncompleteOutput(t *testing.T) {
	s := &Server{Socket: "wrap", R: &fakeRunner{out: "name-only"}}
	if _, _, err := s.ClientSessionIdentity("/dev/ttys002"); err == nil {
		t.Fatal("incomplete client identity should fail")
	}
}

func TestCheckVersion(t *testing.T) {
	cases := []struct {
		out    string
		wantOK bool
	}{
		{"tmux 3.5a", true},
		{"tmux 3.2", true},
		{"tmux 3.1c", false},
		{"tmux 2.9", false},
		{"tmux next-3.6", true},
		{"tmux master", true}, // unparseable → assume new enough
	}
	for _, tc := range cases {
		err := CheckVersion(&fakeRunner{out: tc.out})
		if (err == nil) != tc.wantOK {
			t.Errorf("CheckVersion(%q) err=%v", tc.out, err)
		}
	}
}

func TestHasSessionDistinguishesMissingFromFailure(t *testing.T) {
	s := &Server{Socket: "wrap", R: &fakeRunner{err: errors.New("can't find session: missing")}}
	if got, err := s.HasSession("missing"); err != nil || got {
		t.Fatalf("missing session = %v, %v; want false, nil", got, err)
	}

	s.R = &fakeRunner{err: errors.New("permission denied")}
	if got, err := s.HasSession("maybe"); err == nil || got {
		t.Fatalf("tmux failure = %v, %v; want false with error", got, err)
	}

	s.R = &fakeRunner{err: errors.New("load helper: No such file or directory")}
	if got, err := s.HasSession("maybe"); err == nil || got {
		t.Fatalf("non-socket missing-file failure = %v, %v; want false with error", got, err)
	}
}

func TestKillSessionIDTreatsAlreadyExitedAsClean(t *testing.T) {
	s := &Server{Socket: "wrap", R: &fakeRunner{err: errors.New("can't find session: $7")}}
	if err := s.KillSessionID("$7"); err != nil {
		t.Fatalf("KillSessionID missing target = %v, want nil", err)
	}
}

// Real-tmux round trip on a scratch socket. Skipped when tmux is absent.
func TestIntegrationRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	s := NewServer(fmt.Sprintf("wrap-test-%d-%s", os.Getpid(), strings.ReplaceAll(t.Name(), "/", "-")))
	t.Cleanup(func() {
		if _, err := s.Run("kill-server"); err != nil && !strings.Contains(err.Error(), "no server running") {
			t.Errorf("kill test server: %v", err)
		}
	})
	id, err := s.NewSessionID("it/works", t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.HasSession("it/works"); err != nil || !got {
		t.Fatal("session should exist")
	}
	infos, err := s.Sessions()
	if err != nil || len(infos) != 1 || infos[0].Name != "it/works" || infos[0].ID != id {
		t.Fatalf("Sessions = %+v, %v", infos, err)
	}
	if err := s.RenameSessionID(id, "it/renamed"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.HasSession("it/renamed"); err != nil || !got {
		t.Fatal("renamed session should exist")
	}
	if err := s.KillSessionID(id); err != nil {
		t.Fatal(err)
	}
	if got, err := s.HasSession("it/renamed"); err != nil || got {
		t.Fatal("session should be gone")
	}

	weird := filepath.Join(t.TempDir(), "line\nwith\ttab")
	if err := os.Mkdir(weird, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.NewSessionID("it/weird", weird, ""); err != nil {
		t.Fatal(err)
	}
	canonicalWeird, err := filepath.EvalSymlinks(weird)
	if err != nil {
		t.Fatal(err)
	}
	infos, err = s.Sessions()
	if err != nil || len(infos) != 1 {
		t.Fatalf("Sessions = %+v, %v", infos, err)
	}
	currentPath, err := s.SessionCurrentPath(infos[0].ID)
	if err != nil || currentPath != canonicalWeird {
		t.Fatalf("SessionCurrentPath = %q, %v; want %q", currentPath, err, canonicalWeird)
	}
}
