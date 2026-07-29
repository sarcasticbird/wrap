package launcher

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/state"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

const testGeneration = "0123456789abcdef0123456789abcdef"

type fakeRunner struct {
	calls [][]string
	// hasSessions controls has-session responses by exact target.
	hasSessions map[string]bool
	// hasSessionResults, when non-empty, overrides hasSessions in call
	// order so tests can model a session exiting between two probes.
	hasSessionResults []bool
	hasSessionCalls   int
	paneOut           string
	listOut           string
	displayOut        string // returned for display-message (ClientSession) calls
	displayID         string // stable ID paired with displayOut
	// displayAfterKill, when set, is what display-message returns once a
	// kill-session has been issued. Real tmux with detach-on-destroy off
	// moves the client the instant a session dies, so a canned answer that
	// ignores ordering cannot catch code that asks after the kill.
	displayAfterKill     string
	displayErr           bool // make display-message (ClientSession) fail
	sawKill              bool
	killErr              string // kill-session targeting this exact session name fails
	clientsOut           string // returned for list-clients (ClientTTYs) calls
	failContains         string // fail a command containing this exact substring
	failErr              error  // failure returned for failContains; defaults to errFake
	globalOptions        map[string]string
	sessionGeneration    string
	uiServerPID          string
	uiServerPIDAfterKill string
	uiSessionKilled      bool
	listStarted          chan struct{}
	releaseList          chan struct{}
	listBlocked          bool
	sessionNameMismatch  bool
}

func (f *fakeRunner) Run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	if f.sessionNameMismatch && strings.Contains(joined, "wrap-session-identity-mismatch") {
		return "wrap-session-identity-mismatch", nil
	}
	if f.failContains != "" && strings.Contains(joined, f.failContains) {
		if f.failErr != nil {
			return "", f.failErr
		}
		return "", errFake
	}
	if strings.Contains(joined, "has-session") {
		if f.hasSessionCalls < len(f.hasSessionResults) {
			ok := f.hasSessionResults[f.hasSessionCalls]
			f.hasSessionCalls++
			if ok {
				return "", nil
			}
			return "", errFake
		}
		for name, ok := range f.hasSessions {
			if strings.HasSuffix(joined, "-t ="+name) && ok {
				return "", nil
			}
		}
		return "", errFake
	}
	if strings.Contains(joined, "#{==:#{@wrap_server_generation},}") {
		for _, arg := range args {
			const prefix = "set-option -g @wrap_server_generation "
			if strings.HasPrefix(arg, prefix) && f.sessionGeneration == "" {
				f.sessionGeneration = testGeneration
			}
		}
		return "", nil
	}
	if strings.Contains(joined, "show-options -gvq @wrap_server_generation") {
		return f.sessionGeneration, nil
	}
	if strings.Contains(joined, "#{==:#{@wrap_server_generation},") &&
		(strings.Contains(joined, "kill-session -t") ||
			strings.Contains(joined, "set-option -t") ||
			strings.Contains(joined, "rename-session -t") ||
			strings.Contains(joined, "switch-client -c") ||
			strings.Contains(joined, "new-session -d")) {
		if f.sessionGeneration != "" && !strings.Contains(joined, f.sessionGeneration) {
			return "wrap-server-generation-mismatch", nil
		}
		if strings.Contains(joined, "new-session -d") {
			f.addSession(newSessionName(args, joined), "$7")
			return "$7", nil
		}
		if strings.Contains(joined, "kill-session -t") {
			f.sawKill = true
			targetID := commandTargetID(joined, "kill-session -t ")
			if f.killErr == targetID {
				f.removeSessionID(targetID)
				return "", errFake
			}
			f.removeSessionID(targetID)
		}
		return "", nil
	}
	if strings.Contains(joined, "show-options -gvq") {
		return f.globalOptions[args[len(args)-1]], nil
	}
	if strings.Contains(joined, "set-option -g @wrap_focus_") {
		if f.globalOptions == nil {
			f.globalOptions = map[string]string{}
		}
		f.globalOptions[args[len(args)-2]] = args[len(args)-1]
	}
	if strings.Contains(joined, "new-session") && strings.Contains(joined, "#{session_id}") {
		generation := f.sessionGeneration
		if generation == "" {
			generation = testGeneration
		}
		if strings.Contains(joined, tmux.ServerGenerationOption) {
			return "$7\t" + generation, nil
		}
		return "$new", nil
	}
	if strings.Contains(joined, "-L wrap new-session") {
		f.addSession(newSessionName(args, joined), "$7")
	}
	if f.killErr != "" && strings.Contains(joined, "kill-session") {
		if strings.Contains(joined, "-t ="+f.killErr) || strings.Contains(joined, "-t "+f.killErr) {
			f.removeSessionID(f.killErr)
			return "", errFake
		}
	}
	if strings.Contains(joined, "kill-session") {
		f.sawKill = true
		if strings.Contains(joined, "-L wrap-ui") {
			f.uiSessionKilled = true
		}
	}
	if strings.Contains(joined, "list-clients") {
		return f.clientsOut, nil
	}
	if strings.Contains(joined, "list-panes") {
		return f.paneOut, nil
	}
	if strings.Contains(joined, "list-sessions") {
		if f.listStarted != nil && !f.listBlocked {
			f.listBlocked = true
			close(f.listStarted)
			<-f.releaseList
		}
		if f.listOut == "" {
			var names []string
			for name, alive := range f.hasSessions {
				if alive {
					names = append(names, name)
				}
			}
			sort.Strings(names)
			for i, name := range names {
				f.addSession(name, "$"+strconv.Itoa(i+7))
			}
		}
		return f.listOut, nil
	}
	if strings.Contains(joined, "display-message") {
		if strings.Contains(joined, "#{pid}") {
			if f.uiSessionKilled && f.uiServerPIDAfterKill != "" {
				return f.uiServerPIDAfterKill, nil
			}
			if f.uiServerPID != "" {
				return f.uiServerPID, nil
			}
			return "100", nil
		}
		if target := argAfter(args, "-t"); target != "" {
			for _, line := range strings.Split(f.listOut, "\n") {
				fields := strings.Split(line, "\t")
				if len(fields) >= 12 && fields[7] == target {
					return fields[11], nil
				}
			}
		}
		if f.displayErr {
			return "", errFake
		}
		if strings.Contains(joined, "#{client_session}\t#{session_id}") {
			id := f.displayID
			if id == "" {
				id = "$9"
			}
			return f.displayOut + "\t" + id, nil
		}
		if f.sawKill && f.displayAfterKill != "" {
			return f.displayAfterKill, nil
		}
		return f.displayOut, nil
	}
	return "", nil
}

func commandTargetID(command, prefix string) string {
	rest, ok := strings.CutPrefix(command[strings.Index(command, prefix):], prefix)
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, " ")
	return id
}

func newSessionName(args []string, command string) string {
	if name := argAfter(args, "-s"); name != "" {
		return name
	}
	const marker = " -s "
	index := strings.Index(command, marker)
	if index < 0 {
		return ""
	}
	rest := command[index+len(marker):]
	name, _, _ := strings.Cut(rest, " ")
	return strings.Trim(name, "'")
}

func (f *fakeRunner) addSession(name, id string) {
	if name == "" {
		return
	}
	if f.hasSessions == nil {
		f.hasSessions = map[string]bool{}
	}
	f.hasSessions[name] = true
	for _, line := range strings.Split(f.listOut, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) > 0 && fields[0] == name {
			return
		}
	}
	f.listOut = strings.TrimSpace(f.listOut + "\n" + testSessionLine(name, "", id))
}

func (f *fakeRunner) removeSessionID(id string) {
	if id == "" || f.listOut == "" {
		return
	}
	var kept []string
	for _, line := range strings.Split(f.listOut, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) >= 8 && fields[7] == id {
			continue
		}
		kept = append(kept, line)
	}
	f.listOut = strings.Join(kept, "\n")
}

func argAfter(args []string, want string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == want {
			return args[i+1]
		}
	}
	return ""
}

var errFake = &fakeErr{}

type fakeErr struct{}

func (*fakeErr) Error() string { return "can't find session" }

type ownershipRunner struct {
	active atomic.Bool
}

func (r *ownershipRunner) Run(args ...string) (string, error) {
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "has-session") {
		if r.active.Load() {
			return "", nil
		}
		return "", errFake
	}
	if strings.Contains(joined, "list-sessions") {
		return "", errors.New("no server running")
	}
	return "", nil
}

func (f *fakeRunner) all() string {
	var b strings.Builder
	for _, c := range f.calls {
		b.WriteString(strings.Join(c, " ") + "\n")
	}
	return b.String()
}

func testSessionLine(name, marker, id string) string {
	return testSessionLineAt(name, marker, "", id, "")
}

func testSessionLineAt(name, marker, pathToken, id, currentPath string) string {
	return testSessionLineAtWithKind(name, marker, pathToken, id, currentPath, "")
}

func testSessionLineAtWithKind(name, marker, pathToken, id, currentPath, kind string) string {
	return strings.Join([]string{
		name, "1", "0", "0", "", marker, pathToken, id, testGeneration,
		"1", kind, currentPath,
	}, "\t")
}

func configureMappedEntry(t *testing.T, name, path string) string {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.WriteEntryPaths("vb", map[string]string{name: canonical}); err != nil {
		t.Fatal(err)
	}
	return canonical
}

func newTestManagerWS(f *fakeRunner, ws string) *Manager {
	return &Manager{
		UI:   &tmux.Server{Socket: tmux.SocketUI, ConfigFile: "/dev/null", R: f},
		Sess: &tmux.Server{Socket: tmux.SocketSessions, R: f},
		Exe:  "/bin/wrap",
		WS:   ws,
	}
}

type sessionPathRunner struct {
	responses map[string]string
	calls     []string
	err       error
}

func (r *sessionPathRunner) Run(args ...string) (string, error) {
	command := strings.Join(args, " ")
	r.calls = append(r.calls, command)
	if r.err != nil {
		return "", r.err
	}
	for id, response := range r.responses {
		if strings.Contains(command, "display-message -p -t "+id) {
			return response, nil
		}
	}
	return "", nil
}

func TestSessionCurrentPathUsesGenerationGuard(t *testing.T) {
	runner := &sessionPathRunner{responses: map[string]string{"$7": "/repos/api"}}
	m := &Manager{Sess: &tmux.Server{Socket: tmux.SocketSessions, R: runner}}
	got, err := m.SessionCurrentPath("$7", testGeneration)
	if err != nil || got != "/repos/api" {
		t.Fatalf("SessionCurrentPath = %q, %v", got, err)
	}
	if len(runner.calls) != 1 ||
		!strings.Contains(runner.calls[0], testGeneration) ||
		!strings.Contains(runner.calls[0], "#{pane_current_path}") {
		t.Fatalf("commands = %v, want one generation-guarded path read", runner.calls)
	}
}

func TestSessionCurrentPathAddsOperationContext(t *testing.T) {
	cause := errors.New("permission denied")
	runner := &sessionPathRunner{err: cause}
	m := &Manager{Sess: &tmux.Server{Socket: tmux.SocketSessions, R: runner}}
	_, err := m.SessionCurrentPath("$7", testGeneration)
	if !errors.Is(err, cause) {
		t.Fatalf("SessionCurrentPath error = %v, want wrapped cause", err)
	}
	for _, want := range []string{
		"read session current path",
		"read current path for session $7",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("SessionCurrentPath error = %q, want context %q", err, want)
		}
	}
}

func guardWorkspaceMetaOnce(m *Manager, meta state.Meta) error {
	release, err := m.GuardWorkspaceMeta(meta)
	if err != nil {
		return err
	}
	return release()
}

func TestEnsureSessionServer(t *testing.T) {
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	if err := m.EnsureSessionServer("/launch/dir"); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "-L wrap new-session -d -s wrap-home -c /launch/dir") {
		t.Errorf("home session not created at the launch dir:\n%s", all)
	}
	if !strings.Contains(all, "-L wrap set-option -g detach-on-destroy off") {
		t.Errorf("detach-on-destroy not set:\n%s", all)
	}
	if !strings.Contains(all, "-L wrap set-option -wg monitor-bell on") {
		t.Errorf("monitor-bell not enabled:\n%s", all)
	}
	if !strings.Contains(all, "-L wrap set-option -g set-clipboard on") {
		t.Errorf("set-clipboard not enabled on session server:\n%s", all)
	}
}

func TestNewTerm(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true},
		listOut:     "vb\t1\t0\t0\t\nvb·term·1\t1\t0\t0\t\nvb·term·3\t1\t0\t0\t",
	}
	m := newTestManagerWS(f, "vb")
	name, err := m.NewTerm("/ws/root", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if name != "vb·term·4" {
		t.Errorf("name = %q, want vb·term·4 (max existing + 1)", name)
	}
	if !strings.Contains(f.all(), "new-session -d -P -F '#{session_id}' -s vb·term·4 -c /ws/root claude") {
		t.Errorf("term not created:\n%s", f.all())
	}
	if !strings.Contains(f.all(), "set-option -t $7 "+tmux.SessionKindOption+" "+tmux.SessionKindScratch) {
		t.Errorf("scratch kind not recorded:\n%s", f.all())
	}
}

func TestNewTermRollsBackWhenKindMarkerFails(t *testing.T) {
	f := &fakeRunner{
		hasSessions:  map[string]bool{"wrap-home": true},
		failContains: "set-option -t $7 " + tmux.SessionKindOption,
		failErr:      errors.New("tmux option failure"),
	}
	m := newTestManagerWS(f, "vb")
	_, err := m.NewTerm("/ws/root", "claude")
	if err == nil || !strings.Contains(err.Error(), "mark terminal vb·term·1 as scratch") {
		t.Fatalf("NewTerm error = %v, want scratch marker failure", err)
	}
	if all := f.all(); !strings.Contains(all, "new-session -d -P -F '#{session_id}' -s vb·term·1") ||
		!strings.Contains(all, "kill-session -t $7") {
		t.Fatalf("unmarked scratch session was not rolled back:\n%s", all)
	}
}

func TestEnsureEntrySessionIdempotent(t *testing.T) {
	f := &fakeRunner{hasSessions: map[string]bool{"wrap-home": true, "p/e": true}}
	m := newTestManagerWS(f, "vb")
	if err := m.EnsureEntrySession("p/e", "/tmp/x", "myagent"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.all(), "new-session -d -s p/e") {
		t.Errorf("existing session recreated:\n%s", f.all())
	}
}

func TestEnsureExistingEntrySessionMarksStableID(t *testing.T) {
	dir := t.TempDir()
	canonical := configureMappedEntry(t, "vb/repo", dir)
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "vb/repo": true},
		listOut:     testSessionLineAt("vb/repo", "", "", "$4", dir),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.EnsureEntrySession("vb/repo", dir, "myagent"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "set-option -t $4 "+tmux.EntryNameOption+" "+tmux.EncodeEntryName("vb/repo")) {
		t.Fatalf("existing entry marker did not target stable session id:\n%s", f.all())
	}
	if !strings.Contains(f.all(), "set-option -t $4 "+tmux.SessionKindOption+" "+tmux.SessionKindEntry) {
		t.Fatalf("existing entry kind did not target stable session id:\n%s", f.all())
	}
	if !strings.Contains(f.all(), "set-option -t $4 "+tmux.EntryPathOption+" "+tmux.EncodeEntryPath(canonical)) {
		t.Fatalf("existing entry path marker did not target stable session id:\n%s", f.all())
	}
}

func TestValidateLiveEntrySessionsBackfillsAndPersistsPaths(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	repo := t.TempDir()
	f := &fakeRunner{listOut: strings.Join([]string{
		testSessionLineAt("vb", "", "", "$1", root),
		testSessionLineAt("vb/repo", "", "", "$2", repo),
	}, "\n")}
	m := newTestManagerWS(f, "vb")
	if err := m.ValidateLiveEntrySessions(map[string]string{
		"vb":      root,
		"vb/repo": repo,
	}, true); err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, want := range []string{
		"set-option -t $1 " + tmux.SessionKindOption + " " + tmux.SessionKindEntry,
		"set-option -t $2 " + tmux.SessionKindOption + " " + tmux.SessionKindEntry,
		"set-option -t $1 " + tmux.EntryNameOption + " " + tmux.EncodeEntryName("vb"),
		"set-option -t $2 " + tmux.EntryNameOption + " " + tmux.EncodeEntryName("vb/repo"),
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("missing identity backfill %q:\n%s", want, all)
		}
	}
	paths, ok, err := state.ReadEntryPaths("vb")
	if err != nil || !ok || paths["vb"] != canonicalRoot || paths["vb/repo"] != canonicalRepo {
		t.Fatalf("persisted paths = %#v, ok=%v, err=%v", paths, ok, err)
	}
}

func TestValidateLiveEntrySessionsMigratesLegacyScratchKind(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	f := &fakeRunner{listOut: testSessionLineAt("vb·term·1", "", "", "$7", root)}
	m := newTestManagerWS(f, "vb")
	if err := m.ValidateLiveEntrySessions(map[string]string{"vb": root}, true); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, want := range []string{
		"#{==:#{session_name},vb·term·1}",
		"#{==:#{@wrap_session_kind},}",
		"#{==:#{@wrap_entry_name},}",
		"#{==:#{@wrap_entry_path},}",
		"set-option -t $7 " + tmux.SessionKindOption + " " + tmux.SessionKindScratch,
	} {
		if !strings.Contains(all, want) {
			t.Fatalf("legacy scratch migration missing %q:\n%s", want, all)
		}
	}
}

func TestValidateLiveEntrySessionsRefusesScratchLookingEntryMarker(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	root := t.TempDir()
	f := &fakeRunner{listOut: testSessionLineAt(
		"vb·term·renamed-entry", "vb/repo", "", "$7", root,
	)}
	m := newTestManagerWS(f, "vb")
	err := m.ValidateLiveEntrySessions(map[string]string{"vb": root}, true)
	if err == nil || !strings.Contains(err.Error(), "entry identity markers") {
		t.Fatalf("ValidateLiveEntrySessions error = %v, want marker conflict", err)
	}
	if strings.Contains(f.all(), "set-option -t $7 "+tmux.SessionKindOption) {
		t.Fatalf("scratch-looking entry session was claimed:\n%s", f.all())
	}
}

func TestValidateLiveEntrySessionsRejectsIncompleteTopologyWithoutPublishing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := newTestManagerWS(&fakeRunner{}, "vb")
	err := m.ValidateLiveEntrySessions(map[string]string{"vb": t.TempDir()}, false)
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("err = %v, want incomplete-topology refusal", err)
	}
	if _, ok, readErr := state.ReadEntryPaths("vb"); readErr != nil || ok {
		t.Fatalf("partial entry map published: ok=%v err=%v", ok, readErr)
	}
}

func TestShowInMiddleRefusesConflictingEntryPathBeforeSwitch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	expected := t.TempDir()
	if err := state.WriteEntryPaths("vb", map[string]string{"vb/repo": expected}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		listOut: testSessionLineAt(
			"vb/repo",
			"vb/repo",
			tmux.EncodeEntryPath("/different/repo"),
			"$2",
			"/different/repo",
		),
		paneOut: testPanes,
	}
	m := newTestManagerWS(f, "vb")
	err := m.ShowInMiddle("vb/repo")
	if err == nil || !strings.Contains(err.Error(), "not requested path") {
		t.Fatalf("err = %v, want path identity conflict", err)
	}
	if strings.Contains(f.all(), "switch-client") {
		t.Fatalf("conflicting session was attached:\n%s", f.all())
	}
}

func TestEnsureExistingEntrySessionRefusesMismatchedMarker(t *testing.T) {
	dir := t.TempDir()
	configureMappedEntry(t, "vb/repo", dir)
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "vb/repo": true},
		listOut:     testSessionLine("vb/repo", "vb/repo~20old", "$4"),
	}
	m := newTestManagerWS(f, "vb")
	err := m.EnsureEntrySession("vb/repo", dir, "myagent")
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v, want marker conflict", err)
	}
	if strings.Contains(f.all(), "set-option -t $4 "+tmux.EntryNameOption) {
		t.Fatalf("mismatched marker was overwritten:\n%s", f.all())
	}
}

func TestEnsureExistingEntrySessionRefusesDifferentRecordedPath(t *testing.T) {
	newPath := t.TempDir()
	configureMappedEntry(t, "vb/repo", newPath)
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "vb/repo": true},
		listOut: testSessionLineAt(
			"vb/repo",
			"vb/repo",
			tmux.EncodeEntryPath("/old/repo"),
			"$4",
			"/old/repo",
		),
	}
	m := newTestManagerWS(f, "vb")
	err := m.EnsureEntrySession("vb/repo", newPath, "myagent")
	if err == nil || !strings.Contains(err.Error(), "/old/repo") || !strings.Contains(err.Error(), newPath) {
		t.Fatalf("err = %v, want old/new path identity conflict", err)
	}
	if strings.Contains(f.all(), "new-session -d -P") {
		t.Fatalf("path-conflicting session was replaced implicitly:\n%s", f.all())
	}
}

func TestEnsureLegacyEntrySessionRefusesDifferentCurrentPath(t *testing.T) {
	oldPath := t.TempDir()
	newPath := t.TempDir()
	configureMappedEntry(t, "vb/repo", newPath)
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "vb/repo": true},
		listOut:     testSessionLineAt("vb/repo", "", "", "$4", oldPath),
	}
	m := newTestManagerWS(f, "vb")
	err := m.EnsureEntrySession("vb/repo", newPath, "myagent")
	if err == nil || !strings.Contains(err.Error(), "current path") {
		t.Fatalf("err = %v, want legacy current-path conflict", err)
	}
	if strings.Contains(f.all(), "set-option -t $4 "+tmux.EntryPathOption) {
		t.Fatalf("conflicting legacy session was marked as the new path:\n%s", f.all())
	}
}

func TestEnsureLegacyEntrySessionAcceptsCurrentPathBelowEntryRoot(t *testing.T) {
	root := t.TempDir()
	subdir := filepath.Join(root, "src", "pkg")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical := configureMappedEntry(t, "vb/repo", root)
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "vb/repo": true},
		listOut:     testSessionLineAt("vb/repo", "", "", "$4", subdir),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.EnsureEntrySession("vb/repo", root, "myagent"); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "set-option -t $4 "+tmux.EntryPathOption+" "+tmux.EncodeEntryPath(canonical)) {
		t.Fatalf("descendant working directory was not safely backfilled:\n%s", all)
	}
}

func TestEnsureExistingEntrySessionAcceptsMatchingPathMarker(t *testing.T) {
	dir := t.TempDir()
	canonical := configureMappedEntry(t, "vb/repo", dir)
	token := tmux.EncodeEntryPath(canonical)
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "vb/repo": true},
		listOut: testSessionLineAtWithKind(
			"vb/repo", "vb/repo", token, "$4", "/somewhere/else", tmux.SessionKindEntry,
		),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.EnsureEntrySession("vb/repo", dir, "myagent"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.all(), "set-option -t $4") {
		t.Fatalf("matching identity markers were rewritten:\n%s", f.all())
	}
}

func TestEnsureEntrySessionRollsBackWhenIdentityMarkerFails(t *testing.T) {
	dir := t.TempDir()
	configureMappedEntry(t, "vb/repo", dir)
	f := &fakeRunner{
		hasSessions:  map[string]bool{"wrap-home": true},
		failContains: tmux.EntryNameOption,
		failErr:      errors.New("tmux option failure"),
	}
	m := newTestManagerWS(f, "vb")
	err := m.EnsureEntrySession("vb/repo", dir, "myagent")
	if err == nil || !strings.Contains(err.Error(), "tmux option failure") {
		t.Fatalf("err = %v, want marker failure", err)
	}
	if all := f.all(); !strings.Contains(all, "new-session -d -P -F '#{session_id}' -s vb/repo") ||
		!strings.Contains(all, "kill-session -t $7") {
		t.Fatalf("unmarked new session was not rolled back:\n%s", all)
	}
}

func TestEnsureEntrySessionRefusesSelectionAbsentFromValidatedTopology(t *testing.T) {
	valid := t.TempDir()
	stale := t.TempDir()
	configureMappedEntry(t, "vb/current", valid)
	f := &fakeRunner{hasSessions: map[string]bool{"wrap-home": true}}
	m := newTestManagerWS(f, "vb")
	err := m.EnsureEntrySession("vb/stale", stale, "myagent")
	if err == nil || !strings.Contains(err.Error(), "absent from current workspace discovery") {
		t.Fatalf("err = %v, want stale-selection refusal", err)
	}
	if strings.Contains(f.all(), "new-session") {
		t.Fatalf("stale selection created a session:\n%s", f.all())
	}
}

func TestMigrateLegacyEntrySessionsRenamesUniqueSessionAndSelection(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := config.LegacySessionName("vb", "api server")
	newName := config.SessionName("vb", "api server")
	sel := state.Selection{Entry: "api server", Session: old, Path: "/repos/api"}
	if err := state.Write("vb", sel); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{listOut: testSessionLine(old, "", "$1")}
	m := newTestManagerWS(f, "vb")
	if err := m.MigrateLegacyEntrySessions([]string{"api server"}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "rename-session -t $1 "+newName) {
		t.Fatalf("legacy session was not renamed:\n%s", f.all())
	}
	got, ok, err := state.Read("vb")
	if err != nil || !ok {
		t.Fatalf("Read selection: ok=%v err=%v", ok, err)
	}
	if got.Session != newName || got.Entry != sel.Entry || got.Path != sel.Path {
		t.Fatalf("selection = %+v, want session %q with other fields preserved", got, newName)
	}
}

func TestMigrateLegacyEntrySessionsRefusesRestartedServer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := config.LegacySessionName("vb", "api server")
	f := &fakeRunner{
		listOut:           testSessionLine(old, "", "$1"),
		sessionGeneration: "fedcba9876543210fedcba9876543210",
	}
	m := newTestManagerWS(f, "vb")
	err := m.MigrateLegacyEntrySessions([]string{"api server"}, true)
	if !errors.Is(err, tmux.ErrServerGenerationChanged) {
		t.Fatalf("migration after restart = %v, want generation change", err)
	}
	if strings.Contains(f.all(), "rename-session -t") {
		t.Fatalf("reused session ID was renamed:\n%s", f.all())
	}
}

func TestMigrateLegacyEntrySessionsPathConflictMutatesNothing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	entry := "api server"
	old := config.LegacySessionName("vb", entry)
	newName := config.SessionName("vb", entry)
	expected := t.TempDir()
	stale := t.TempDir()
	before := state.Selection{Entry: entry, Session: old, Path: expected}
	if err := state.Write("vb", before); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		listOut: testSessionLineAt(old, "", "", "$1", stale),
	}
	m := newTestManagerWS(f, "vb")
	err := m.MigrateLegacyEntrySessionsWithPaths(
		[]string{entry},
		map[string]string{newName: expected},
		true,
	)
	if err == nil || !strings.Contains(err.Error(), "outside requested entry path") {
		t.Fatalf("err = %v, want path ownership conflict", err)
	}
	all := f.all()
	if strings.Contains(all, "set-option") || strings.Contains(all, "rename-session") {
		t.Fatalf("path-conflicting legacy session was mutated:\n%s", all)
	}
	after, ok, readErr := state.Read("vb")
	if readErr != nil || !ok || after != before {
		t.Fatalf("selection changed after failed preflight: before=%+v after=%+v ok=%v err=%v", before, after, ok, readErr)
	}
}

func TestMigrateLegacyEntrySessionsRejectsConflictingSessionKind(t *testing.T) {
	for _, kind := range []string{tmux.SessionKindScratch, tmux.SessionKindDiff} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			entry := "api server"
			old := config.LegacySessionName("vb", entry)
			newName := config.SessionName("vb", entry)
			expected := t.TempDir()
			f := &fakeRunner{listOut: testSessionLineAtWithKind(
				old, "", "", "$1", expected, kind,
			)}
			m := newTestManagerWS(f, "vb")
			err := m.MigrateLegacyEntrySessionsWithPaths(
				[]string{entry},
				map[string]string{newName: expected},
				true,
			)
			if err == nil || !strings.Contains(err.Error(), "refusing entry migration") ||
				!strings.Contains(err.Error(), kind) {
				t.Fatalf("migration error = %v, want conflicting %s kind", err, kind)
			}
			all := f.all()
			if strings.Contains(all, "set-option") || strings.Contains(all, "rename-session") {
				t.Fatalf("conflicting-kind session was mutated:\n%s", all)
			}
		})
	}
}

func TestMigrateLegacyEntrySessionsRefusesAmbiguousCollision(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := config.LegacySessionName("vb", "foo.bar")
	f := &fakeRunner{listOut: testSessionLine(old, "", "$1")}
	m := newTestManagerWS(f, "vb")
	err := m.MigrateLegacyEntrySessions([]string{"foo.bar", "foo:bar"}, true)
	if err == nil || !strings.Contains(err.Error(), "ambiguous legacy session") {
		t.Fatalf("err = %v, want explicit ambiguity", err)
	}
	if strings.Contains(f.all(), "rename-session") {
		t.Fatalf("ambiguous migration mutated tmux:\n%s", f.all())
	}
}

func TestMigrateLegacyEntrySessionsRefusesExistingNewSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	entry := "api server"
	old := config.LegacySessionName("vb", entry)
	newName := config.SessionName("vb", entry)
	f := &fakeRunner{listOut: strings.Join([]string{
		testSessionLine(old, "", "$1"),
		testSessionLine(newName, "", "$2"),
	}, "\n")}
	m := newTestManagerWS(f, "vb")
	err := m.MigrateLegacyEntrySessions([]string{entry}, true)
	if err == nil || !strings.Contains(err.Error(), "both exist") {
		t.Fatalf("err = %v, want explicit existing-target conflict", err)
	}
	if strings.Contains(f.all(), "rename-session") {
		t.Fatalf("conflicting migration mutated tmux:\n%s", f.all())
	}
}

func TestMigrateLegacyEntrySessionsRepairsSelectionAfterPartialPriorRun(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	entry := "api server"
	old := config.LegacySessionName("vb", entry)
	newName := config.SessionName("vb", entry)
	if err := state.Write("vb", state.Selection{Entry: entry, Session: old, Path: "/repos/api"}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{listOut: testSessionLine(newName, "", "$1")}
	m := newTestManagerWS(f, "vb")
	if err := m.MigrateLegacyEntrySessions([]string{entry}, true); err != nil {
		t.Fatal(err)
	}
	got, ok, err := state.Read("vb")
	if err != nil || !ok || got.Session != newName {
		t.Fatalf("selection = %+v, ok=%v err=%v; want repaired session %q", got, ok, err, newName)
	}
	if !strings.Contains(f.all(), "set-option -t $1 "+tmux.EntryNameOption+" "+tmux.EncodeEntryName(newName)) {
		t.Fatalf("markerless partial migration was not backfilled:\n%s", f.all())
	}
	if strings.Contains(f.all(), "rename-session") {
		t.Fatalf("already migrated session was renamed again:\n%s", f.all())
	}
}

func TestMigrateLegacyEntrySessionsRefusesSelectionForHistoricalCollider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := config.LegacySessionName("vb", "foo:bar")
	if err := state.Write("vb", state.Selection{
		Entry: "foo:bar", Session: old, Path: "/old/foo-colon",
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{listOut: testSessionLine(old, "", "$1")}
	m := newTestManagerWS(f, "vb")
	err := m.MigrateLegacyEntrySessions([]string{"foo.bar"}, true)
	if err == nil || !strings.Contains(err.Error(), "saved selection") {
		t.Fatalf("err = %v, want historical-selection conflict", err)
	}
	if strings.Contains(f.all(), "rename-session") {
		t.Fatalf("historical selection was rebound to a different row:\n%s", f.all())
	}
}

func TestMigrateLegacyEntrySessionsRefusesHistoricalSelectionCollidingWithUnchangedName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := config.LegacySessionName("vb", "foo.bar")
	if err := state.Write("vb", state.Selection{
		Entry: "foo.bar", Session: old, Path: "/old/foo-dot",
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{listOut: testSessionLine(old, "", "$1")}
	m := newTestManagerWS(f, "vb")
	err := m.MigrateLegacyEntrySessions([]string{"foo_bar"}, true)
	if err == nil || !strings.Contains(err.Error(), "saved selection") {
		t.Fatalf("err = %v, want unchanged-name identity conflict", err)
	}
	if strings.Contains(f.all(), "rename-session") {
		t.Fatalf("historical selection was rebound to literal underscore row:\n%s", f.all())
	}
}

func TestMigrateLegacyEntrySessionsMarkerPreventsSecondEncoding(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := config.LegacySessionName("vb", "foo.bar")
	first := config.SessionName("vb", "foo.bar")
	second := config.SessionName("vb", "foo~2ebar")
	entries := []string{"foo.bar", "foo~2ebar"}

	f := &fakeRunner{listOut: testSessionLine(old, "", "$1")}
	m := newTestManagerWS(f, "vb")
	if err := m.MigrateLegacyEntrySessions(entries, true); err != nil {
		t.Fatal(err)
	}
	if all := f.all(); !strings.Contains(all, "set-option -t $1 "+tmux.EntryNameOption+" "+tmux.EncodeEntryName(first)) ||
		!strings.Contains(all, "rename-session -t $1 "+first) {
		t.Fatalf("first migration did not mark then rename:\n%s", all)
	}

	f2 := &fakeRunner{listOut: testSessionLine(first, first, "$2")}
	m2 := newTestManagerWS(f2, "vb")
	if err := m2.MigrateLegacyEntrySessions(entries, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f2.all(), "rename-session -t $2 "+second) {
		t.Fatalf("encoded session was encoded a second time:\n%s", f2.all())
	}
}

func TestMigrateLegacyEntrySessionsBackfillsCanonicalSessionBeforeCollider(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first := config.SessionName("vb", "foo.bar")
	second := config.SessionName("vb", "foo~2ebar")
	if err := state.Write("vb", state.Selection{
		Entry: "foo.bar", Session: first, Path: "/repos/foo-dot",
	}); err != nil {
		t.Fatal(err)
	}

	f := &fakeRunner{listOut: testSessionLine(first, "", "$1")}
	m := newTestManagerWS(f, "vb")
	if err := m.MigrateLegacyEntrySessions([]string{"foo.bar"}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "set-option -t $1 "+tmux.EntryNameOption+" "+tmux.EncodeEntryName(first)) {
		t.Fatalf("canonical session was not backfilled:\n%s", f.all())
	}

	f2 := &fakeRunner{listOut: testSessionLine(first, first, "$2")}
	m2 := newTestManagerWS(f2, "vb")
	if err := m2.MigrateLegacyEntrySessions([]string{"foo.bar", "foo~2ebar"}, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f2.all(), "rename-session -t $2 "+second) {
		t.Fatalf("backfilled canonical session was treated as collider legacy:\n%s", f2.all())
	}
}

func TestMigrateLegacyEntrySessionsLeavesUnprovenCanonicalSessionUnmarked(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	name := config.SessionName("vb", "foo.bar")
	f := &fakeRunner{listOut: testSessionLine(name, "", "$1")}
	m := newTestManagerWS(f, "vb")
	if err := m.MigrateLegacyEntrySessions([]string{"foo.bar"}, true); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.all(), "set-option -t $1 "+tmux.EntryNameOption) {
		t.Fatalf("unproven ambiguous canonical name was marked:\n%s", f.all())
	}
}

func TestMigrateLegacyEntrySessionsDoesNotBackfillFirstLaunchColliderWithoutSelection(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	first := config.SessionName("vb", "foo.bar")
	f := &fakeRunner{listOut: testSessionLine(first, "", "$1")}
	m := newTestManagerWS(f, "vb")
	err := m.MigrateLegacyEntrySessions([]string{"foo.bar", "foo~2ebar"}, true)
	if err == nil || !strings.Contains(err.Error(), "could be encoded") {
		t.Fatalf("err = %v, want unresolved encoded/legacy ambiguity", err)
	}
	if strings.Contains(f.all(), tmux.EntryNameOption) &&
		strings.Contains(f.all(), "set-option") {
		t.Fatalf("ambiguous first-launch session was marked:\n%s", f.all())
	}
}

func TestMigrateLegacyEntrySessionsRetriesMarkedRenameAfterColliderAppears(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := config.LegacySessionName("vb", "foo.bar")
	target := config.SessionName("vb", "foo.bar")

	f := &fakeRunner{
		listOut:      testSessionLine(old, "", "$1"),
		failContains: "rename-session",
		failErr:      errors.New("tmux rename failure"),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.MigrateLegacyEntrySessions([]string{"foo.bar"}, true); err == nil {
		t.Fatal("expected first rename to fail after marker write")
	}
	if !strings.Contains(f.all(), "set-option -t $1 "+tmux.EntryNameOption+" "+tmux.EncodeEntryName(target)) {
		t.Fatalf("failed migration did not leave a retry marker:\n%s", f.all())
	}

	f2 := &fakeRunner{listOut: testSessionLine(old, target, "$2")}
	m2 := newTestManagerWS(f2, "vb")
	if err := m2.MigrateLegacyEntrySessions([]string{"foo.bar", "foo_bar"}, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f2.all(), "rename-session -t $2 "+target) {
		t.Fatalf("pending marked migration did not resume:\n%s", f2.all())
	}
}

func TestMigrateLegacyEntrySessionsRequiresCompleteTopologyForLiveMigration(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := config.LegacySessionName("vb", "foo.bar")
	f := &fakeRunner{listOut: testSessionLine(old, "", "$1")}
	m := newTestManagerWS(f, "vb")
	err := m.MigrateLegacyEntrySessions([]string{"foo.bar"}, false)
	if err == nil || !strings.Contains(err.Error(), "incomplete entry discovery") {
		t.Fatalf("err = %v, want incomplete-topology blocker", err)
	}
	if strings.Contains(f.all(), "rename-session") {
		t.Fatalf("partial topology authorized a migration:\n%s", f.all())
	}
}

func TestMigrateLegacyEntrySessionsAllowsIncompleteTopologyWithoutLiveLegacyCandidate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{listOut: testSessionLine("wrap-home", "", "$1")}
	m := newTestManagerWS(f, "vb")
	if err := m.MigrateLegacyEntrySessions([]string{"foo.bar"}, false); err != nil {
		t.Fatalf("incomplete topology without a migration candidate blocked launch: %v", err)
	}
}

// TestShowInMiddle pins that ShowInMiddle switches the middle pane's
// nested client WITHOUT moving pane focus — used by the terms pane,
// which switches what the middle pane shows but keeps focus in place.
func TestShowInMiddle(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "p/e": true},
		// The terminal pane (the one every side pane points its nested
		// client at) is discovered by its " attach " start command — here
		// %2/dev/ttys003 — not by a fixed index.
		paneOut: "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShowInMiddle("p/e"); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "switch-client -c /dev/ttys003 -t $7") {
		t.Errorf("switch-client wrong:\n%s", all)
	}
	if strings.Contains(all, "select-pane") {
		t.Errorf("ShowInMiddle should not move pane focus:\n%s", all)
	}
}

func TestShowInMiddleUsesValidatedStableSessionID(t *testing.T) {
	dir := t.TempDir()
	canonical := configureMappedEntry(t, "vb/repo", dir)
	f := &fakeRunner{
		listOut: testSessionLineAt(
			"vb/repo",
			"vb/repo",
			tmux.EncodeEntryPath(canonical),
			"$7",
			canonical,
		),
		paneOut: testPanes,
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShowInMiddle("vb/repo"); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "switch-client -c /dev/ttys003 -t $7") {
		t.Fatalf("entry switch did not target validated stable id:\n%s", all)
	}
	if !strings.Contains(all, "set-option -t $7 "+tmux.BellOption+" 0") {
		t.Fatalf("post-switch option mutation did not target validated stable id:\n%s", all)
	}
	if strings.Contains(all, "switch-client -c /dev/ttys003 -t =vb/repo") {
		t.Fatalf("entry switch fell back to racy name target:\n%s", all)
	}
}

func TestShowInMiddleUsesStableScratchIDForSwitchAndBellClear(t *testing.T) {
	f := &fakeRunner{
		listOut: testSessionLine("vb·term·1", "", "$7") + "\n" +
			testSessionLine("vb·term·10", "", "$8"),
		paneOut: testPanes,
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShowInMiddle("vb·term·1"); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, command := range []string{
		"switch-client -c /dev/ttys003 -t $7",
		"set-option -t $7 " + tmux.BellOption + " 0",
	} {
		if !strings.Contains(all, command) {
			t.Fatalf("scratch mutation did not target stable id with %q:\n%s", command, all)
		}
	}
	if strings.Contains(all, "-t vb·term·1 ") || strings.Contains(all, "-t =vb·term·1") {
		t.Fatalf("scratch mutation used prefix-matchable name:\n%s", all)
	}
}

// TestSwitchMiddle pins that SwitchMiddle (used by the tree pane, which
// owns focus transfer on selection) does both: switches the client AND
// focuses the middle pane.
func TestSwitchMiddle(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "p/e": true},
		// The terminal pane is %2 — SwitchMiddle must focus it by pane_id,
		// not by a fixed window-relative index.
		paneOut: "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.SwitchMiddle("p/e"); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "switch-client -c /dev/ttys003 -t $7") {
		t.Errorf("switch-client wrong:\n%s", all)
	}
	if !strings.Contains(all, "-f /dev/null -L wrap-ui select-pane -t %2") {
		t.Errorf("middle pane not focused by pane_id:\n%s", all)
	}
}

func TestSwitchMiddleMatchesExactAttachCommand(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "p/e": true},
		paneOut:     "%0\t/dev/ttys001\t'/tmp/has attach marker/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/tmp/has attach marker/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/tmp/has attach marker/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	m.Exe = "/tmp/has attach marker/wrap"
	if err := m.SwitchMiddle("p/e"); err != nil {
		t.Fatal(err)
	}
	if all := f.all(); !strings.Contains(all, "switch-client -c /dev/ttys003 -t $7") {
		t.Fatalf("terminal pane selected by substring instead of exact command:\n%s", all)
	}
}

// TestShowDiffUnstagedCommand pins the plain working-tree diff form (not
// staged, not untracked).
func TestShowDiffUnstagedCommand(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShowDiff("/repo/root", "internal/foo.go", false, false); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, want := range []string{"new-session -d -P -F '#{session_id}' -s vb·diff -c /repo/root", "--literal-pathspecs diff --no-ext-diff --no-textconv --color --", "internal/foo.go", "| less -R"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

// TestShowDiffStagedCommand pins the --cached form.
func TestShowDiffStagedCommand(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShowDiff("/repo/root", "internal/foo.go", true, false); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, want := range []string{"new-session -d -P -F '#{session_id}' -s vb·diff -c /repo/root", "--color --cached --", "internal/foo.go", "| less -R"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
	if !strings.Contains(all, "set-option -t $7 "+tmux.SessionKindOption+" "+tmux.SessionKindDiff) {
		t.Errorf("diff kind not recorded:\n%s", all)
	}
}

// TestShowDiffUntrackedCommand pins the --no-index-against-/dev/null form.
func TestShowDiffUntrackedCommand(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShowDiff("/repo/root", "new/file.go", false, true); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, want := range []string{"new-session -d -P -F '#{session_id}' -s vb·diff -c /repo/root", "--color --no-index -- /dev/null", "new/file.go", "| less -R"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

func TestShowDiffRollsBackWhenKindMarkerFails(t *testing.T) {
	f := &fakeRunner{
		hasSessions:  map[string]bool{"wrap-home": true},
		paneOut:      testPanes,
		failContains: "set-option -t $7 " + tmux.SessionKindOption,
		failErr:      errors.New("tmux option failure"),
	}
	m := newTestManagerWS(f, "vb")
	err := m.ShowDiff("/repo/root", "foo.go", false, false)
	if err == nil || !strings.Contains(err.Error(), "mark diff session vb·diff") {
		t.Fatalf("ShowDiff error = %v, want diff marker failure", err)
	}
	if all := f.all(); !strings.Contains(all, "new-session -d -P -F '#{session_id}' -s vb·diff") ||
		!strings.Contains(all, "kill-session -t $7") {
		t.Fatalf("unmarked diff session was not rolled back:\n%s", all)
	}
}

// TestShowDiffKillsExistingSession covers the not-cleanly-quit-last-time
// case: a stale vb·diff session must be killed before the new one is
// created, not left to error out new-session.
func TestShowDiffKillsExistingSession(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "vb·diff": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
		listOut:     testSessionLine("vb·diff", "", "$9"),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShowDiff("/repo/root", "foo.go", false, false); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "kill-session -t $9") {
		t.Errorf("stale diff session not killed:\n%s", all)
	}
	if !strings.Contains(all, "set-option -t $9 "+tmux.SessionKindOption+" "+tmux.SessionKindDiff) {
		t.Errorf("legacy diff session was not marked before replacement:\n%s", all)
	}
	killIdx := strings.Index(all, "kill-session -t $9")
	newIdx := strings.Index(all, "new-session -d -P -F '#{session_id}' -s vb·diff")
	if killIdx == -1 || newIdx == -1 || killIdx > newIdx {
		t.Errorf("kill must happen before recreate:\n%s", all)
	}
}

// TestShowDiffSwitchesMiddleWithFocus covers that ShowDiff uses
// SwitchMiddle (not ShowInMiddle) — the pager takes focus.
func TestShowDiffSwitchesMiddleWithFocus(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShowDiff("/repo/root", "foo.go", false, false); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "switch-client -c /dev/ttys003 -t $7") {
		t.Errorf("switch-client wrong:\n%s", all)
	}
	if !strings.Contains(all, "select-pane -t %2") {
		t.Errorf("middle pane not focused by pane_id:\n%s", all)
	}
}

func TestShowDiffStaleDiffKillFailureStopsReplacement(t *testing.T) {
	f := &fakeRunner{
		hasSessions:  map[string]bool{"wrap-home": true, "vb·diff": true},
		paneOut:      "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
		listOut:      testSessionLineAtWithKind("vb·diff", "", "", "$9", "", tmux.SessionKindDiff),
		failContains: "kill-session -t $9",
		failErr:      errors.New("permission denied"),
	}
	m := newTestManagerWS(f, "vb")
	err := m.ShowDiff("/repo/root", "foo.go", false, false)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("ShowDiff error = %v, want stale-session kill failure", err)
	}
	all := f.all()
	if !strings.Contains(all, "kill-session -t $9") {
		t.Errorf("kill-session not attempted:\n%s", all)
	}
	if strings.Contains(all, "new-session -d -s vb·diff") {
		t.Errorf("replacement was attempted after uncertain cleanup:\n%s", all)
	}
}

func TestShowDiffConcurrentStaleExitStillRecreates(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
		listOut:     testSessionLineAtWithKind("vb·diff", "", "", "$9", "", tmux.SessionKindDiff),
		killErr:     "$9",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShowDiff("/repo/root", "foo.go", false, false); err != nil {
		t.Fatalf("already-exited diff blocked replacement: %v", err)
	}
	if !strings.Contains(f.all(), "new-session -d -P -F '#{session_id}' -s vb·diff") {
		t.Fatalf("diff was not recreated after concurrent exit:\n%s", f.all())
	}
}

func TestShowDiffRefusesReusedIDAfterServerRestart(t *testing.T) {
	f := &fakeRunner{
		hasSessions:       map[string]bool{"wrap-home": true},
		paneOut:           testPanes,
		listOut:           testSessionLineAtWithKind("vb·diff", "", "", "$9", "", tmux.SessionKindDiff),
		sessionGeneration: "fedcba9876543210fedcba9876543210",
	}
	m := newTestManagerWS(f, "vb")
	err := m.ShowDiff("/repo/root", "foo.go", false, false)
	if !errors.Is(err, tmux.ErrServerGenerationChanged) {
		t.Fatalf("ShowDiff after restart = %v, want generation change", err)
	}
	if strings.Contains(f.all(), "new-session -d -s vb·diff") {
		t.Fatalf("replacement diff was created after uncertain cleanup:\n%s", f.all())
	}
}

// TestLaunchUIBuildsChrome covers chrome naming for a named workspace.
// Every workspace is folder-based now, so there is no separate "global"
// naming case to test — this is the only chrome-naming shape.
//
// Pane indices after the two splits: tree=0, terms=1 (bottom-left,
// created by the second split which targets the tree pane again),
// terminal=2 (right column, full height) — see LaunchUI's doc comment.
func TestLaunchUIBuildsChrome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{hasSessions: map[string]bool{}, paneOut: ""}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, want := range []string{
		"-f /dev/null -L wrap-ui new-session -d -s wrap-vb -x 220 -y 60 '/bin/wrap' sidebar 'vb'",
		"-f /dev/null -L wrap-ui split-window -h -t =wrap-vb:0.0 -l 75% '/bin/wrap' attach 'vb'",
		"-f /dev/null -L wrap-ui split-window -v -t =wrap-vb:0.0 -l 30% '/bin/wrap' watch 'vb'",
		"-f /dev/null -L wrap-ui set-option -g status off",
		"-f /dev/null -L wrap-ui set-option -g prefix C-q",
		"-f /dev/null -L wrap-ui set-option -g mouse on",
		"-f /dev/null -L wrap-ui set-option -g set-clipboard on",
		"-f /dev/null -L wrap-ui set-option -t wrap-vb @wrap_tree_side left",
		"-f /dev/null -L wrap-ui bind-key -n M-2 if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .1 select-pane -t .0",
		"-f /dev/null -L wrap-ui bind-key -n M-1 if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .0 select-pane -t .2",
		"-f /dev/null -L wrap-ui bind-key -n M-3 if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .2 select-pane -t .1",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

// TestLaunchUIIdempotent pins the reattach path as a round trip: whatever
// LaunchUI persists on a build must compare equal to the same params on
// the very next run, so a second launch reattaches instead of rebuilding.
//
// Asserting the round trip rather than a hand-written chrome.json is what
// catches normalization drift — if LaunchUI ever defaults or clamps a
// field after the comparison instead of before it, the stored and
// compared values diverge and every launch tears the chrome down.
func TestLaunchUIIdempotent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	p := state.ChromeParams{TreeSide: "left", TreeWidth: 25}
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-vb": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	// First launch: no chrome.json yet, so this builds and persists.
	if err := m.LaunchUI(p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "new-session") {
		t.Fatalf("first launch should have built the chrome:\n%s", f.all())
	}
	// Second launch, identical params: must reattach, touching nothing.
	f.calls = nil
	if err := m.LaunchUI(p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.all(), "new-session") || strings.Contains(f.all(), "kill-session") {
		t.Errorf("chrome rebuilt on relaunch with unchanged params:\n%s", f.all())
	}
}

// Chrome built by an older wrap must be torn down and rebuilt, even when
// every workspace-level param still matches.
//
// The panes are long-lived processes running whatever binary built them, so
// reuse pins a workspace to the old code forever: the bell work was live in
// the binary and invisible in every already-open workspace. A chrome.json
// written before ChromeBuild existed has no "build" key and decodes to 0,
// which is the case this exercises.
func TestLaunchUIRebuildsChromeFromOlderBuild(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-vb": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	p := state.ChromeParams{TreeSide: "left", TreeWidth: 25}
	if err := m.LaunchUI(p); err != nil {
		t.Fatal(err)
	}
	// Rewrite the persisted params as an older wrap would have left them:
	// identical in every way except the build stamp.
	stored, ok, err := state.ReadChromeParams("vb")
	if err != nil || !ok {
		t.Fatalf("ReadChromeParams: %v ok=%v", err, ok)
	}
	if stored.Build != state.ChromeBuild {
		t.Fatalf("Build = %d, want %d", stored.Build, state.ChromeBuild)
	}
	stored.Build = 0
	if err := state.WriteChromeParams("vb", stored); err != nil {
		t.Fatal(err)
	}

	f.calls = nil
	if err := m.LaunchUI(p); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "kill-session") || !strings.Contains(f.all(), "new-session") {
		t.Errorf("chrome from an older build was reused instead of rebuilt:\n%s", f.all())
	}
	// And the rebuild must restamp, or every subsequent launch rebuilds.
	after, _, err := state.ReadChromeParams("vb")
	if err != nil {
		t.Fatal(err)
	}
	if after.Build != state.ChromeBuild {
		t.Errorf("after rebuild Build = %d, want %d", after.Build, state.ChromeBuild)
	}
}

// LaunchUI defaults empty keys before it compares, so a config that spells
// out the defaults explicitly is not treated as a change.
func TestLaunchUIExplicitDefaultKeysDoNotForceRebuild(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-vb": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	// Built with no keys set (so the defaults apply)...
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	// ...then relaunched with those same bindings written out longhand.
	f.calls = nil
	explicit := state.ChromeParams{TreeSide: "left", TreeWidth: 25,
		Keys: config.Keys{FocusTree: "M-2", FocusTerminal: "M-1", FocusTerms: "M-3"}}
	if err := m.LaunchUI(explicit); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.all(), "kill-session") {
		t.Errorf("explicit default keys forced a rebuild:\n%s", f.all())
	}
}

// TestLaunchUIRebuildsOnParamMismatch pins that a healthy 3-pane chrome
// whose stored chrome.json DIFFERS from the freshly-resolved params (here,
// tree_width changed) is torn down and rebuilt, and the new params are
// persisted.
func TestLaunchUIRebuildsOnParamMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	old := state.ChromeParams{TreeSide: "left", TreeWidth: 25}
	if err := state.WriteChromeParams("vb", old); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-vb": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	next := state.ChromeParams{TreeSide: "left", TreeWidth: 40}
	// LaunchUI normalizes before it compares and persists, so the stored
	// params carry the defaulted keys and the build stamp the chrome was
	// actually built with.
	wantStored := next
	wantStored.Keys = next.Keys.WithDefaults()
	wantStored.Build = state.ChromeBuild
	if err := m.LaunchUI(next); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "-f /dev/null -L wrap-ui kill-session -t =wrap-vb") {
		t.Errorf("mismatched-params chrome not torn down:\n%s", all)
	}
	if !strings.Contains(all, "new-session -d -s wrap-vb") {
		t.Errorf("chrome not rebuilt on param mismatch:\n%s", all)
	}
	got, ok, err := state.ReadChromeParams("vb")
	if err != nil || !ok || got != wantStored {
		t.Errorf("stored params not updated: got %+v ok=%v err=%v, want %+v", got, ok, err, wantStored)
	}
}

// TestLaunchUIRebuildsOnMissingChromeParams pins the upgrade path: a
// healthy 3-pane chrome that predates this feature (no chrome.json on
// disk at all) is rebuilt once rather than trusted blind, and chrome.json
// is then written so the next run can reattach normally.
func TestLaunchUIRebuildsOnMissingChromeParams(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-vb": true},
		paneOut:     "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	p := state.ChromeParams{TreeSide: "left", TreeWidth: 25}
	// LaunchUI normalizes before it compares and persists, so the stored
	// params carry the defaulted keys and the build stamp the chrome was
	// actually built with.
	wantStored := p
	wantStored.Keys = p.Keys.WithDefaults()
	wantStored.Build = state.ChromeBuild
	if err := m.LaunchUI(p); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "-f /dev/null -L wrap-ui kill-session -t =wrap-vb") {
		t.Errorf("pre-upgrade chrome not torn down:\n%s", all)
	}
	if !strings.Contains(all, "new-session -d -s wrap-vb") {
		t.Errorf("chrome not rebuilt on the upgrade path:\n%s", all)
	}
	if got, ok, err := state.ReadChromeParams("vb"); err != nil || !ok || got != wantStored {
		t.Errorf("chrome params not written after upgrade rebuild: got %+v ok=%v err=%v", got, ok, err)
	}
}

func TestLaunchUIRebuildsDeadChrome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-vb": true},
		// Only 2 panes reported (terms pane dead) — liveness check
		// (len(panes) == 3) must trip a rebuild.
		paneOut: "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "-f /dev/null -L wrap-ui kill-session -t =wrap-vb") {
		t.Errorf("broken chrome not torn down:\n%s", all)
	}
	if !strings.Contains(all, "new-session -d -s wrap-vb") {
		t.Errorf("chrome not rebuilt:\n%s", all)
	}
}

func TestLaunchUIPaneQueryFailureDoesNotKillLiveChrome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions:  map[string]bool{"wrap-vb": true},
		failContains: "list-panes",
		failErr:      errors.New("tmux transport failed"),
	}
	m := newTestManagerWS(f, "vb")
	err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25})
	if err == nil || !strings.Contains(err.Error(), "tmux transport failed") {
		t.Fatalf("LaunchUI error = %v, want pane query failure", err)
	}
	if strings.Contains(f.all(), "kill-session") {
		t.Fatalf("unconfirmed pane state destroyed live chrome:\n%s", f.all())
	}
}

func TestLaunchUIMissingWindowRebuildsConfirmedBrokenChrome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions:  map[string]bool{"wrap-vb": true},
		failContains: "list-panes",
		failErr:      errors.New("can't find window: 0"),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatalf("LaunchUI: %v", err)
	}
	all := f.all()
	if !strings.Contains(all, "kill-session -t =wrap-vb") || !strings.Contains(all, "new-session -d -s wrap-vb") {
		t.Fatalf("confirmed missing chrome window was not rebuilt:\n%s", all)
	}
}

func TestLaunchUISessionExitBetweenProbesBuildsWithoutAbsentKill(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessionResults: []bool{true, false},
		failContains:      "list-panes",
		failErr:           errors.New("can't find session: wrap-vb"),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatalf("LaunchUI: %v", err)
	}
	all := f.all()
	if strings.Contains(all, "kill-session -t =wrap-vb") {
		t.Fatalf("already-exited chrome was killed by stale name:\n%s", all)
	}
	if !strings.Contains(all, "new-session -d -s wrap-vb") {
		t.Fatalf("chrome was not rebuilt after concurrent exit:\n%s", all)
	}
}

func TestLaunchUISessionExitBeforePIDProbeBuildsFresh(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessionResults: []bool{true, false},
		failContains:      "display-message -p #{pid}",
		failErr:           errors.New("no server running"),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatalf("LaunchUI: %v", err)
	}
	all := f.all()
	if strings.Contains(all, "kill-session -t =wrap-vb") {
		t.Fatalf("already-exited chrome was killed after failed PID probe:\n%s", all)
	}
	if !strings.Contains(all, "new-session -d -s wrap-vb") {
		t.Fatalf("chrome was not rebuilt after PID-probe exit:\n%s", all)
	}
}

func TestLaunchUIUnreadableChromeParamsDoesNotKillLiveChrome(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	stateDir := filepath.Join(stateHome, "wrap", "vb")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "chrome.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-vb": true},
		paneOut:     "%0\t/dev/ttys001\ttree\n%1\t/dev/ttys002\twatch\n%2\t/dev/ttys003\tattach",
	}
	m := newTestManagerWS(f, "vb")
	err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25})
	if err == nil || !strings.Contains(err.Error(), "chrome params") {
		t.Fatalf("LaunchUI error = %v, want chrome params read failure", err)
	}
	if strings.Contains(f.all(), "kill-session") {
		t.Fatalf("unreadable persisted state destroyed live chrome:\n%s", f.all())
	}
}

func TestLaunchUICustomKeys(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25, Keys: config.Keys{FocusTree: "M-a"}}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "bind-key -n M-a if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .1 select-pane -t .0") {
		t.Errorf("custom key not bound:\n%s", all)
	}
	if !strings.Contains(all, "bind-key -n M-1 if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .0 select-pane -t .2") {
		t.Errorf("unset keys should default:\n%s", all)
	}
}

func TestLaunchUIUnbindsSupersededGlobalFocusKeys(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions: map[string]bool{},
		globalOptions: map[string]string{
			focusTreeOption:     "M-a",
			focusTerminalOption: "M-b",
			focusTermsOption:    "M-c",
		},
	}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, old := range []string{"M-a", "M-b", "M-c"} {
		if !strings.Contains(all, "unbind-key -n "+old) {
			t.Errorf("superseded key %s was not unbound:\n%s", old, all)
		}
	}
	for option, want := range map[string]string{
		focusTreeOption: "M-2", focusTerminalOption: "M-1", focusTermsOption: "M-3",
	} {
		if got := f.globalOptions[option]; got != want {
			t.Errorf("%s = %q, want %q", option, got, want)
		}
	}
}

func TestLaunchUIReconcilesGlobalFocusKeysForHealthyChrome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	params := state.ChromeParams{
		Build:     state.ChromeBuild,
		TreeSide:  "left",
		TreeWidth: 25,
		Keys:      config.Keys{}.WithDefaults(),
	}
	if err := state.WriteChromeParams("vb", params); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-vb": true},
		paneOut:     testPanes,
		globalOptions: map[string]string{
			focusTreeOption:     "M-a",
			focusTerminalOption: "M-b",
			focusTermsOption:    "M-c",
		},
	}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(params); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if strings.Contains(all, "kill-session") || strings.Contains(all, "new-session") {
		t.Fatalf("healthy chrome was rebuilt while reconciling global keys:\n%s", all)
	}
	for _, old := range []string{"M-a", "M-b", "M-c"} {
		if !strings.Contains(all, "unbind-key -n "+old) {
			t.Errorf("superseded global key %s was not unbound:\n%s", old, all)
		}
	}
	for option, want := range map[string]string{
		focusTreeOption: "M-2", focusTerminalOption: "M-1", focusTermsOption: "M-3",
	} {
		if got := f.globalOptions[option]; got != want {
			t.Errorf("%s = %q, want %q", option, got, want)
		}
	}
}

func TestConfigureFocusKeysAlwaysCleansLiveServerMarkers(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		globalOptions: map[string]string{
			focusTreeOption:     "M-a",
			focusTerminalOption: "M-b",
			focusTermsOption:    "M-c",
		},
	}
	m := newTestManagerWS(f, "vb")
	if err := m.configureFocusKeys(config.Keys{}.WithDefaults(), config.Keys{}, false); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, old := range []string{"M-a", "M-b", "M-c"} {
		if !strings.Contains(all, "unbind-key -n "+old) {
			t.Errorf("live server marker %s was not cleaned without persisted-key cleanup:\n%s", old, all)
		}
	}
}

func TestLaunchUIMigratesFocusKeysFromPreMarkerChrome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.WriteChromeParams("vb", state.ChromeParams{
		Build: 7, TreeSide: "left", TreeWidth: 25,
		Keys: config.Keys{FocusTree: "M-a", FocusTerminal: "M-b", FocusTerms: "M-c"},
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-vb": true},
		paneOut:     testPanes,
		globalOptions: map[string]string{
			focusTreeOption:     "M-x",
			focusTerminalOption: "M-y",
			focusTermsOption:    "M-z",
		},
	}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, old := range []string{"M-a", "M-b", "M-c", "M-x", "M-y", "M-z"} {
		if !strings.Contains(all, "unbind-key -n "+old) {
			t.Errorf("historical key %s was not migrated:\n%s", old, all)
		}
	}
}

func TestLaunchUIMigratesStoredKeysFromBrokenChrome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.WriteChromeParams("vb", state.ChromeParams{
		Build: 7, Keys: config.Keys{FocusTree: "M-a", FocusTerminal: "M-b", FocusTerms: "M-c"},
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-vb": true},
		paneOut:     "%0\t/dev/ttys001\ttree\n%1\t/dev/ttys002\twatch",
	}
	if err := newTestManagerWS(f, "vb").LaunchUI(state.ChromeParams{}); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"M-a", "M-b", "M-c"} {
		if !strings.Contains(f.all(), "unbind-key -n "+old) {
			t.Errorf("broken chrome did not clean stored key %s:\n%s", old, f.all())
		}
	}
}

func TestLaunchUIMigratesStoredKeysWhenWorkspaceSessionIsMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.WriteChromeParams("vb", state.ChromeParams{
		Build: 7, Keys: config.Keys{FocusTree: "M-a", FocusTerminal: "M-b", FocusTerms: "M-c"},
	}); err != nil {
		t.Fatal(err)
	}
	// wrap-vb is absent, but ServerPID succeeds because another workspace
	// still owns the shared UI server and its global key table.
	f := &fakeRunner{hasSessions: map[string]bool{}, uiServerPID: "100"}
	if err := newTestManagerWS(f, "vb").LaunchUI(state.ChromeParams{}); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"M-a", "M-b", "M-c"} {
		if !strings.Contains(f.all(), "unbind-key -n "+old) {
			t.Errorf("missing workspace chrome did not clean shared key %s:\n%s", old, f.all())
		}
	}
}

func TestLaunchUISkipsStoredKeyCleanupWhenNoUIServerExists(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.WriteChromeParams("vb", state.ChromeParams{
		Build: 7, Keys: config.Keys{FocusTree: "M-a", FocusTerminal: "M-b", FocusTerms: "M-c"},
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions:  map[string]bool{},
		failContains: "display-message -p #{pid}",
		failErr:      errors.New("no server running on /tmp/wrap-ui"),
	}
	if err := newTestManagerWS(f, "vb").LaunchUI(state.ChromeParams{}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.all(), "unbind-key") {
		t.Fatalf("fresh UI server received historical cleanup:\n%s", f.all())
	}
}

func TestLaunchUISkipsHistoricalCleanupAfterServerRestart(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.WriteChromeParams("vb", state.ChromeParams{
		Build: 7, Keys: config.Keys{FocusTree: "M-a", FocusTerminal: "M-b", FocusTerms: "M-c"},
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions:          map[string]bool{"wrap-vb": true},
		paneOut:              testPanes,
		uiServerPID:          "100",
		uiServerPIDAfterKill: "200",
		globalOptions:        map[string]string{focusTreeOption: "M-x"},
	}
	if err := newTestManagerWS(f, "vb").LaunchUI(state.ChromeParams{}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, persisted := range []string{"M-a", "M-b", "M-c"} {
		if strings.Contains(all, "unbind-key -n "+persisted) {
			t.Fatalf("fresh UI server received persisted historical cleanup for %s:\n%s", persisted, all)
		}
	}
	if !strings.Contains(all, "unbind-key -n M-x") {
		t.Fatalf("live server marker was not reconciled after restart:\n%s", all)
	}
}

func TestLaunchUIIgnoresAlreadyMissingHistoricalBinding(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions:   map[string]bool{},
		globalOptions: map[string]string{focusTreeOption: "M-a"},
		failContains:  "unbind-key -n M-a",
		failErr:       errors.New("unknown key: M-a"),
	}
	if err := newTestManagerWS(f, "vb").LaunchUI(state.ChromeParams{}); err != nil {
		t.Fatalf("already-missing cleanup should be idempotent: %v", err)
	}
}

// TestLaunchUIRightSideCommands pins the mirrored "right" tree_side build
// sequence: split-window -h -b (position AND index flip) puts the
// terminal at index 0 and the tree at index 1, then split-window -v -t
// <win>.1 splits the (now relocated) tree pane to produce terms at index
// 2. Binds follow the same mapping: sidebar(tree)=.1, terminal=.0,
// watcher(terms)=.2.
func TestLaunchUIRightSideCommands(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "right", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, want := range []string{
		"-f /dev/null -L wrap-ui new-session -d -s wrap-vb -x 220 -y 60 '/bin/wrap' sidebar 'vb'",
		"-f /dev/null -L wrap-ui split-window -h -b -t =wrap-vb:0.0 -l 75% '/bin/wrap' attach 'vb'",
		"-f /dev/null -L wrap-ui split-window -v -t =wrap-vb:0.1 -l 30% '/bin/wrap' watch 'vb'",
		"-f /dev/null -L wrap-ui set-option -t wrap-vb @wrap_tree_side right",
		"-f /dev/null -L wrap-ui bind-key -n M-2 if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .1 select-pane -t .0",
		"-f /dev/null -L wrap-ui bind-key -n M-1 if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .0 select-pane -t .2",
		"-f /dev/null -L wrap-ui bind-key -n M-3 if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .2 select-pane -t .1",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

func TestLaunchUIFocusBindingsRemainValidAcrossOppositeLayouts(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{hasSessions: map[string]bool{}}
	if err := newTestManagerWS(f, "left").LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	if err := newTestManagerWS(f, "right").LaunchUI(state.ChromeParams{TreeSide: "right", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "set-option -t wrap-left @wrap_tree_side left") ||
		!strings.Contains(all, "set-option -t wrap-right @wrap_tree_side right") {
		t.Fatalf("layout was not recorded per session:\n%s", all)
	}
	for _, key := range []string{"M-1", "M-2", "M-3"} {
		if got := strings.Count(all, "bind-key -n "+key+" if-shell -F #{==:#{@wrap_tree_side},right}"); got != 2 {
			t.Errorf("%s dynamic binding count = %d, want one identical install per workspace", key, got)
		}
	}
}

// TestLaunchUIEmptyTreeSideDefaultsLeft pins that an empty/unspecified
// treeSide builds the left-shaped (unmirrored) chrome.
func TestLaunchUIEmptyTreeSideDefaultsLeft(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "split-window -h -t =wrap-vb:0.0 -l 75% '/bin/wrap' attach 'vb'") {
		t.Errorf("empty tree_side should default to the left layout:\n%s", all)
	}
	if strings.Contains(all, "-b") {
		t.Errorf("left layout should not use -b:\n%s", all)
	}
}

// TestLaunchUICustomTreeWidth pins that the terminal split's -l percentage
// is 100-treeWidth.
func TestLaunchUICustomTreeWidth(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 40}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "split-window -h -t =wrap-vb:0.0 -l 60% '/bin/wrap' attach 'vb'") {
		t.Errorf("treeWidth 40 should give a 60%% terminal split:\n%s", all)
	}
}

func TestLaunchUIRightSideCustomWidth(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "right", TreeWidth: 40}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	// treeWidth 40 on right should give 60% terminal split with -b flag
	if !strings.Contains(all, "-l 60%") {
		t.Errorf("treeWidth 40 should give a 60%% terminal split:\n%s", all)
	}
	if !strings.Contains(all, "split-window -h -b -t =wrap-vb:0.0 -l 60%") {
		t.Errorf("right layout should have -b flag for split:\n%s", all)
	}
	// Check binds match right-side table: .1 for sidebar, .0 for terminal, .2 for watcher
	for _, want := range []string{
		"bind-key -n M-2 if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .1 select-pane -t .0",
		"bind-key -n M-1 if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .0 select-pane -t .2",
		"bind-key -n M-3 if-shell -F #{==:#{@wrap_tree_side},right} select-pane -t .2 select-pane -t .1",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

// TestLaunchUIRealTmuxLayoutBothSides is the required real-tmux
// integration test: it builds the actual chrome (production LaunchUI,
// unmodified) on a scratch socket for BOTH tree_side layouts and asserts
// tmux's own pane geometry matches the verified mapping — no fakes, no
// reimplementation of the split logic.
func TestLaunchUIRealTmuxLayoutBothSides(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stub := filepath.Join(t.TempDir(), "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, side := range []string{"left", "right"} {
		t.Run(side, func(t *testing.T) {
			sock := "wrap-test-layout-" + side
			ui := tmux.NewServer(sock)
			ui.ConfigFile = "/dev/null"
			m := &Manager{UI: ui, Sess: tmux.NewServer(sock), Exe: stub, WS: "layout"}
			t.Cleanup(func() { _, _ = ui.Run("kill-server") })

			// Run the default width (25%) per the addendum: "run it with
			// the default width."
			if err := m.LaunchUI(state.ChromeParams{TreeSide: side, TreeWidth: 25}); err != nil {
				t.Fatal(err)
			}

			out, err := m.UI.Run("list-panes", "-t", m.uiWindow(), "-F",
				"#{window_height}\t#{pane_id}\t#{pane_tty}\t#{pane_left}\t#{pane_top}\t#{pane_height}\t#{pane_start_command}")
			if err != nil {
				t.Fatal(err)
			}

			type pane struct {
				id, tty, cmd      string
				left, top, height int
			}
			var windowHeight int
			var panes []pane
			for _, line := range strings.Split(out, "\n") {
				f := strings.SplitN(line, "\t", 7)
				if len(f) != 7 {
					continue
				}
				wh, _ := strconv.Atoi(f[0])
				left, _ := strconv.Atoi(f[3])
				top, _ := strconv.Atoi(f[4])
				height, _ := strconv.Atoi(f[5])
				windowHeight = wh
				panes = append(panes, pane{id: f[1], tty: f[2], left: left, top: top, height: height, cmd: f[6]})
			}
			if len(panes) != 3 {
				t.Fatalf("expected 3 panes, got %d:\n%s", len(panes), out)
			}

			var attachPane *pane
			var others []pane
			for i := range panes {
				p := panes[i]
				if strings.Contains(p.cmd, " attach ") {
					attachPane = &panes[i]
				} else {
					others = append(others, p)
				}
			}
			if attachPane == nil {
				t.Fatalf("no pane's start command contained \" attach \":\n%s", out)
			}
			if attachPane.height != windowHeight {
				t.Errorf("attach pane height = %d, want the full window height %d (full column)", attachPane.height, windowHeight)
			}
			if len(others) != 2 {
				t.Fatalf("expected 2 non-attach panes, got %d", len(others))
			}

			// Identify TREE pane (sidebar) and TERMS pane (watcher)
			var treePane, termsPane *pane
			for i := range others {
				if strings.Contains(others[i].cmd, " sidebar ") {
					treePane = &others[i]
				} else if strings.Contains(others[i].cmd, " watch ") {
					termsPane = &others[i]
				}
			}
			if treePane == nil {
				t.Fatalf("no non-attach pane's command contained \" sidebar \":\n%s", out)
			}
			if termsPane == nil {
				t.Fatalf("no non-attach pane's command contained \" watch \":\n%s", out)
			}

			switch side {
			case "left":
				for _, o := range others {
					if o.left >= attachPane.left {
						t.Errorf("left layout: other pane left=%d should be < attach pane left=%d", o.left, attachPane.left)
					}
				}
			case "right":
				if attachPane.left != 0 {
					t.Errorf("right layout: attach pane left = %d, want 0", attachPane.left)
				}
				for _, o := range others {
					if o.left == 0 {
						t.Errorf("right layout: no other pane should be at left=0 (that's the attach pane's column)")
					}
				}
			}
			if treePane.top >= termsPane.top {
				t.Errorf("tree pane should be above terms pane: tree=%+v, terms=%+v", treePane, termsPane)
			}

			tty, err := m.middleTTY()
			if err != nil {
				t.Fatal(err)
			}
			if tty != attachPane.tty {
				t.Errorf("middleTTY() = %q, want the attach pane's tty %q", tty, attachPane.tty)
			}
		})
	}
}

// TestLaunchUIRealTmuxRebuildsOnParamChange is the real-tmux integration
// test for the config-changed-rebuild behavior: it builds the actual
// chrome (production LaunchUI, unmodified) on a scratch socket, then
// relaunches with a changed tree_side and asserts the chrome was torn
// down and rebuilt with the attach pane now on the new side (geometry
// re-checked via tmux itself); a third relaunch with IDENTICAL params
// must instead reattach — the pane ids from right after the rebuild must
// be exactly what they still are after that third call.
func TestLaunchUIRealTmuxRebuildsOnParamChange(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	stub := filepath.Join(t.TempDir(), "stub.sh")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	const sock = "wrap-test-rebuild"
	ui := tmux.NewServer(sock)
	ui.ConfigFile = "/dev/null"
	m := &Manager{UI: ui, Sess: tmux.NewServer(sock), Exe: stub, WS: "layout"}
	t.Cleanup(func() { _, _ = ui.Run("kill-server") })

	attachPaneLeft := func() int {
		out, err := m.UI.Run("list-panes", "-t", m.uiWindow(), "-F", "#{pane_left}\t#{pane_start_command}")
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(out, "\n") {
			f := strings.SplitN(line, "\t", 2)
			if len(f) == 2 && strings.Contains(f[1], " attach ") {
				left, _ := strconv.Atoi(f[0])
				return left
			}
		}
		t.Fatalf("no attach pane found in:\n%s", out)
		return -1
	}

	paneIDs := func() []string {
		panes, err := m.UI.Panes(m.uiWindow())
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]string, len(panes))
		for i, p := range panes {
			ids[i] = p.ID
		}
		return ids
	}

	sameIDs := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}

	// 1. Build the left-layout chrome.
	left := state.ChromeParams{TreeSide: "left", TreeWidth: 25}
	if err := m.LaunchUI(left); err != nil {
		t.Fatal(err)
	}
	if got := attachPaneLeft(); got == 0 {
		t.Fatalf("left layout: attach pane left = %d, want > 0 (tree/terms occupy the left column)", got)
	}

	// 2. Relaunch with tree_side=right: the resolved params now differ
	// from what the chrome was built with, so it must be torn down and
	// rebuilt — the attach pane moves to the LEFT of the window.
	right := state.ChromeParams{TreeSide: "right", TreeWidth: 25}
	if err := m.LaunchUI(right); err != nil {
		t.Fatal(err)
	}
	if got := attachPaneLeft(); got != 0 {
		t.Errorf("right layout: attach pane left = %d, want 0 (rebuild did not apply the new tree_side)", got)
	}
	idsAfterRebuild := paneIDs()

	// 3. A third relaunch with IDENTICAL params must reattach, not
	// rebuild: the pane ids must be exactly what they were right after
	// the rebuild in step 2.
	if err := m.LaunchUI(right); err != nil {
		t.Fatal(err)
	}
	idsAfterReattach := paneIDs()
	if !sameIDs(idsAfterRebuild, idsAfterReattach) {
		t.Errorf("pane ids changed on a no-op relaunch with identical params: before=%v after=%v", idsAfterRebuild, idsAfterReattach)
	}
}

const testPanes = "%0\t/dev/ttys001\t'/bin/wrap' sidebar 'vb'\n%1\t/dev/ttys002\t'/bin/wrap' watch 'vb'\n%2\t/dev/ttys003\t'/bin/wrap' attach 'vb'"

func configureValidLanding(t *testing.T, f *fakeRunner, name string) {
	t.Helper()
	path := t.TempDir()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.WriteEntryPaths("vb", map[string]string{name: canonical}); err != nil {
		t.Fatal(err)
	}
	f.listOut = testSessionLineAt(
		name,
		name,
		tmux.EncodeEntryPath(canonical),
		"$7",
		canonical,
	)
}

func TestKillScratchSessionRejectsNonScratchBeforeTmux(t *testing.T) {
	f := &fakeRunner{}
	m := newTestManagerWS(f, "vb")
	err := m.KillScratchSession("vb/repo", "$9", testGeneration, "")
	if err == nil || err.Error() != "only scratch terminals can be killed" {
		t.Fatalf("KillScratchSession error = %v, want non-scratch refusal", err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("KillScratchSession issued tmux calls before refusing: %v", f.calls)
	}
}

func TestKillScratchSessionRefusesRenamedProtectedTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		paneOut:             testPanes,
		displayOut:          "other/x",
		displayID:           "$other",
		sessionGeneration:   testGeneration,
		sessionNameMismatch: true,
	}
	m := newTestManagerWS(f, "vb")
	err := m.KillScratchSession("vb·term·1", "$9", testGeneration, "")
	if err == nil || !strings.Contains(err.Error(), "changed after confirmation") {
		t.Fatalf("KillScratchSession renamed target error = %v", err)
	}
	if f.sawKill {
		t.Fatal("KillScratchSession killed a target renamed into a protected session")
	}
}

// The redirect decision must be made BEFORE the kill. tmux (with
// detach-on-destroy off) moves the client the moment the session dies, so
// displayAfterKill here reproduces that hop: code that asks afterwards
// sees "other-ws/gamma", concludes the client was never on the killed
// session, and skips the redirect — leaving the user staring at a
// different workspace's terminal.
func TestKillEntrySessionAsksWhatIsShowingBeforeKilling(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions:      map[string]bool{},
		paneOut:          testPanes,
		displayOut:       "p/e",            // showing the session about to die
		displayAfterKill: "other-ws/gamma", // tmux's own MRU hop
	}
	m := newTestManagerWS(f, "vb")
	if err := m.KillEntrySession("p/e", "$9", testGeneration, ""); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "kill-session -t $9") {
		t.Errorf("session not killed:\n%s", all)
	}
	if strings.Contains(all, "kill-session -t =p/e") {
		t.Errorf("kill targeted reusable name instead of captured stable id:\n%s", all)
	}
	if !strings.Contains(all, "switch-client -c /dev/ttys003 -t $7") {
		t.Errorf("terminal pane not redirected after killing what it showed:\n%s", all)
	}
	if strings.Contains(all, "-t =other-ws/gamma") {
		t.Errorf("redirected to tmux's MRU pick instead of a workspace session:\n%s", all)
	}
}

// Killing a session the terminal pane is NOT showing must leave that pane
// alone — the user is looking at something else and it should not move.
func TestKillEntrySessionLeavesTerminalAloneWhenNotShowing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions: map[string]bool{},
		paneOut:     testPanes,
		displayOut:  "other/x",
		displayID:   "$other",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.KillEntrySession("p/e", "$9", testGeneration, "vb/next"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.all(), "switch-client") {
		t.Errorf("unexpected redirect:\n%s", f.all())
	}
}

func TestKillEntrySessionRedirectsRenamedCapturedSessionByID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions: map[string]bool{},
		paneOut:     testPanes,
		displayOut:  "renamed-after-confirmation",
		displayID:   "$9",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.KillEntrySession("p/e", "$9", testGeneration, ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "switch-client -c /dev/ttys003 -t $7") {
		t.Errorf("renamed captured session was not redirected by stable ID:\n%s", f.all())
	}
}

func TestKillEntrySessionRefusesReusedIDAfterServerRestart(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		paneOut:           testPanes,
		displayOut:        "unrelated-replacement",
		displayID:         "$9",
		sessionGeneration: "fedcba9876543210fedcba9876543210",
	}
	m := newTestManagerWS(f, "vb")
	err := m.KillEntrySession("p/e", "$9", testGeneration, "")
	if !errors.Is(err, tmux.ErrServerGenerationChanged) {
		t.Fatalf("KillEntrySession after restart = %v, want generation change", err)
	}
	if f.sawKill {
		t.Fatalf("reused session ID was killed after server restart:\n%s", f.all())
	}
}

func TestKillEntrySessionAddsContextToGuardedKillFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		paneOut:      testPanes,
		displayOut:   "p/e",
		failContains: "kill-session -t $9",
		failErr:      errors.New("tmux transport failed"),
	}
	m := newTestManagerWS(f, "vb")
	err := m.KillEntrySession("p/e", "$9", testGeneration, "")
	if err == nil || !strings.Contains(err.Error(), "kill session p/e by stable ID") ||
		!strings.Contains(err.Error(), "tmux transport failed") {
		t.Fatalf("guarded kill error = %v, want operation and target context", err)
	}
}

// A live successor is where the pane lands.
func TestKillEntrySessionLandsOnSuccessor(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions:      map[string]bool{"vb/next": true},
		paneOut:          testPanes,
		displayOut:       "p/e",
		displayAfterKill: "other-ws/gamma",
	}
	m := newTestManagerWS(f, "vb")
	configureValidLanding(t, f, "vb/next")
	if err := m.KillEntrySession("p/e", "$9", testGeneration, "vb/next"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "switch-client -c /dev/ttys003 -t $7") {
		t.Errorf("did not land on the successor:\n%s", f.all())
	}
}

// A successor that died between x and y falls back to the workspace root
// rather than to tmux's pick.
func TestKillEntrySessionFallsBackToWorkspaceRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions:      map[string]bool{"vb": true}, // root alive, successor gone
		paneOut:          testPanes,
		displayOut:       "p/e",
		displayAfterKill: "other-ws/gamma",
	}
	m := newTestManagerWS(f, "vb")
	configureValidLanding(t, f, "vb")
	if err := m.KillEntrySession("p/e", "$9", testGeneration, "vb/dead"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "switch-client -c /dev/ttys003 -t $7") {
		t.Errorf("did not fall back to the workspace root:\n%s", f.all())
	}
}

func TestPaneCommandsAreSingleArgs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, call := range f.calls {
		for _, arg := range call {
			switch arg {
			case "'/bin/wrap' sidebar 'vb'", "'/bin/wrap' attach 'vb'", "'/bin/wrap' watch 'vb'":
				if arg != call[len(call)-1] {
					t.Errorf("pane command %q is not the final argument: %v", arg, call)
				}
				found++
			}
		}
	}
	if found != 3 {
		t.Errorf("expected 3 single-element pane commands, found %d\ncalls: %v", found, f.calls)
	}
}

func TestShq(t *testing.T) {
	cases := []struct{ in, want string }{
		{"vb", "'vb'"},
		{"/bin/wrap", "'/bin/wrap'"},
		{"o'brien", `'o'\''brien'`},
	}
	for _, c := range cases {
		if got := shq(c.in); got != c.want {
			t.Errorf("shq(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLaunchUIWorkspaceNameShellSafe pins I3: a workspace name containing
// a shell metacharacter must not break out of the pane command tmux
// splices into its own shell.
func TestLaunchUIWorkspaceNameShellSafe(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "o'brien")
	if err := m.LaunchUI(state.ChromeParams{TreeSide: "left", TreeWidth: 25}); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	want := `'/bin/wrap' sidebar 'o'\''brien'`
	if !strings.Contains(all, want) {
		t.Errorf("workspace name with a single quote not shell-quoted safely:\n%s\nwant substring: %s", all, want)
	}
}

func TestPickAttachTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	// No state file → home.
	if got, err := pickAttachTarget("vb", func(string) (bool, error) { return true, nil }); err != nil || got != HomeSession {
		t.Errorf("no state: %q", got)
	}
}

func TestPickAttachTargetRestoresExisting(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.Write("vb", state.Selection{Session: "p/e"}); err != nil {
		t.Fatal(err)
	}
	if got, err := pickAttachTarget("vb", func(n string) (bool, error) { return n == "p/e", nil }); err != nil || got != "p/e" {
		t.Errorf("existing session should be restored, got %q", got)
	}
	if got, err := pickAttachTarget("vb", func(string) (bool, error) { return false, nil }); err != nil || got != HomeSession {
		t.Errorf("gone session should fall back to home, got %q", got)
	}
}

func TestValidatedAttachTargetUsesStableEntryID(t *testing.T) {
	dir := t.TempDir()
	canonical := configureMappedEntry(t, "vb/repo", dir)
	if err := state.Write("vb", state.Selection{
		Entry: "repo", Session: "vb/repo", Path: canonical,
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions: map[string]bool{"vb/repo": true},
		listOut: testSessionLineAt(
			"vb/repo",
			"vb/repo",
			tmux.EncodeEntryPath(canonical),
			"$7",
			canonical,
		),
	}
	m := newTestManagerWS(f, "vb")
	target, err := m.validatedAttachTarget()
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != "$7" || target.Generation != testGeneration {
		t.Fatalf("validated attach target = %+v, want stable identity $7/%s", target, testGeneration)
	}
}

func TestValidatedAttachTargetCapturesHomeIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		listOut: testSessionLine("wrap-home", "", "$3"),
	}
	m := newTestManagerWS(f, "vb")
	target, err := m.validatedAttachTarget()
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != "wrap-home" || target.ID != "$3" || target.Generation != testGeneration {
		t.Fatalf("home attach target = %+v, want wrap-home/$3/%s", target, testGeneration)
	}
}

func TestPickAttachTargetSurfacesMalformedState(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	dir := filepath.Join(stateHome, "wrap", "vb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := pickAttachTarget("vb", func(string) (bool, error) { return false, nil }); err == nil {
		t.Fatal("malformed selection was treated as an absent attach target")
	} else if got := err.Error(); !strings.Contains(got, "vb") || !strings.Contains(got, "attach target") {
		t.Fatalf("err = %q, want workspace and attach-target context", got)
	}
}

func TestGuardWorkspaceMeta(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	metaA := state.Meta{Kind: "folder", Root: "/a/api"}
	metaB := state.Meta{Kind: "folder", Root: "/b/api"}

	// Fresh name: writes.
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "api")
	if err := guardWorkspaceMetaOnce(m, metaA); err != nil {
		t.Fatal(err)
	}
	if got, ok, _ := state.ReadMeta("api"); !ok || got != metaA {
		t.Fatalf("meta not written: %+v ok=%v", got, ok)
	}

	// Same meta, chrome alive: fine (reattach path).
	f.hasSessions["wrap-api"] = true
	if err := guardWorkspaceMetaOnce(m, metaA); err != nil {
		t.Errorf("same-meta reattach should pass: %v", err)
	}

	// Different root, chrome alive: refused, meta untouched.
	if err := guardWorkspaceMetaOnce(m, metaB); err == nil {
		t.Error("live collision should be refused")
	}
	if got, _, _ := state.ReadMeta("api"); got != metaA {
		t.Errorf("meta clobbered on refused guard: %+v", got)
	}

	// Different root, chrome dead: overwrite wins.
	f.hasSessions["wrap-api"] = false
	if err := guardWorkspaceMetaOnce(m, metaB); err != nil {
		t.Errorf("dead chrome should allow takeover: %v", err)
	}
	if got, _, _ := state.ReadMeta("api"); got != metaB {
		t.Errorf("meta not taken over: %+v", got)
	}
}

func TestGuardWorkspaceMetaSerializesConcurrentBasenameOwners(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	runner := &ownershipRunner{}
	manager := func() *Manager {
		return &Manager{
			UI:   &tmux.Server{Socket: tmux.SocketUI, ConfigFile: "/dev/null", R: runner},
			Sess: &tmux.Server{Socket: tmux.SocketSessions, R: runner},
			Exe:  "/bin/wrap",
			WS:   "api",
		}
	}
	firstRelease, err := manager().GuardWorkspaceMeta(state.Meta{Kind: "folder", Root: "/a/api"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = firstRelease() })

	type result struct {
		release func() error
		err     error
	}
	started := make(chan struct{})
	done := make(chan result, 1)
	go func() {
		close(started)
		release, err := manager().GuardWorkspaceMeta(state.Meta{Kind: "folder", Root: "/b/api"})
		done <- result{release: release, err: err}
	}()
	<-started
	select {
	case got := <-done:
		if got.release != nil {
			_ = got.release()
		}
		t.Fatal("second basename owner passed the guard before the first launch became visible")
	case <-time.After(100 * time.Millisecond):
	}

	// This is the point at which runLaunch has created its UI session.
	runner.active.Store(true)
	if err := firstRelease(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.release != nil {
			_ = got.release()
		}
		if got.err == nil || !strings.Contains(got.err.Error(), "already running") {
			t.Fatalf("second owner result = %v, want collision after serialized recheck", got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second owner did not resume after the first released its launch lock")
	}
}

func TestGuardWorkspaceMetaRefusesSurvivingOwnedSessions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	legacy := state.Meta{Kind: "folder", Root: "/old/api"}
	if err := state.WriteMeta("api", legacy); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions: map[string]bool{},
		listOut:     "api/repo\t1\t0\t0\t0",
	}
	m := newTestManagerWS(f, "api")
	if err := guardWorkspaceMetaOnce(m, state.Meta{Kind: "folder", Root: "/new/api"}); err == nil {
		t.Fatal("surviving workspace-owned session should block metadata takeover")
	}
	if got, _, _ := state.ReadMeta("api"); got != legacy {
		t.Fatalf("legacy metadata was overwritten: %+v", got)
	}
}

func TestGuardWorkspaceMetaSurfacesTmuxFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.WriteMeta("api", state.Meta{Kind: "folder", Root: "/old/api"}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions:  map[string]bool{},
		failContains: "has-session",
		failErr:      errors.New("permission denied"),
	}
	m := newTestManagerWS(f, "api")
	if err := guardWorkspaceMetaOnce(m, state.Meta{Kind: "folder", Root: "/new/api"}); err == nil ||
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("tmux ownership check failure was treated as absence: %v", err)
	}
}

func TestGuardWorkspaceMetaTreatsTypedNoServerAsInactive(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.WriteMeta("api", state.Meta{Kind: "folder", Root: "/old/api"}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions:  map[string]bool{},
		failContains: "list-sessions",
		failErr:      errors.New("tmux list-sessions: no server running"),
	}
	m := newTestManagerWS(f, "api")
	current := state.Meta{Kind: "folder", Root: "/new/api"}
	if err := guardWorkspaceMetaOnce(m, current); err != nil {
		t.Fatalf("absent session server blocked inactive takeover: %v", err)
	}
	if got, ok, err := state.ReadMeta("api"); err != nil || !ok || got != current {
		t.Fatalf("metadata = %+v, ok=%v err=%v; want takeover", got, ok, err)
	}
}

func TestGuardWorkspaceMetaPreservesUnrelatedMissingFileFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.WriteMeta("api", state.Meta{Kind: "folder", Root: "/old/api"}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions:  map[string]bool{},
		failContains: "list-sessions",
		failErr:      errors.New("load helper: No such file or directory"),
	}
	m := newTestManagerWS(f, "api")
	if err := guardWorkspaceMetaOnce(m, state.Meta{Kind: "folder", Root: "/new/api"}); err == nil ||
		!strings.Contains(err.Error(), "load helper") {
		t.Fatalf("unrelated missing-file failure was treated as no-server: %v", err)
	}
}

func TestGuardWorkspaceMetaMigratesCanonicalEquivalentRootOnlyWhenStopped(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	realRoot := t.TempDir()
	parent := t.TempDir()
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(alias)
	if err != nil {
		t.Fatal(err)
	}
	legacy := state.Meta{Kind: "folder", Root: alias}
	current := state.Meta{Kind: "folder", Root: canonical}
	if err := state.WriteMeta("alias", legacy); err != nil {
		t.Fatal(err)
	}

	f := &fakeRunner{hasSessions: map[string]bool{"wrap-alias": true}}
	m := newTestManagerWS(f, "alias")
	if err := guardWorkspaceMetaOnce(m, current); err == nil {
		t.Fatal("live legacy workspace should require an explicit stop before migration")
	}
	if got, ok, err := state.ReadMeta("alias"); err != nil || !ok || got != legacy {
		t.Fatalf("live meta = %+v, ok=%v, err=%v; want untouched legacy meta %+v", got, ok, err, legacy)
	}

	f.hasSessions["wrap-alias"] = false
	if err := guardWorkspaceMetaOnce(m, current); err != nil {
		t.Fatalf("stopped legacy workspace should migrate: %v", err)
	}
	if got, ok, err := state.ReadMeta("alias"); err != nil || !ok || got != current {
		t.Fatalf("migrated meta = %+v, ok=%v, err=%v; want canonical meta %+v", got, ok, err, current)
	}
}

// TestRenameTermHappyPath pins the rename-session command string and the
// returned new session name.
func TestRenameTermHappyPath(t *testing.T) {
	f := &fakeRunner{listOut: testSessionLine("vb·term·1", "", "$1")}
	m := newTestManagerWS(f, "vb")
	name, err := m.RenameTerm("vb·term·1", "$1", testGeneration, "logs")
	if err != nil {
		t.Fatal(err)
	}
	if name != "vb·term·logs" {
		t.Errorf("name = %q, want vb·term·logs", name)
	}
	if !strings.Contains(f.all(), "rename-session -t $1 vb·term·logs") {
		t.Errorf("rename-session not issued:\n%s", f.all())
	}
}

func TestRenameTermRefusesChangedIdentity(t *testing.T) {
	f := &fakeRunner{
		listOut:             testSessionLine("vb·term·1", "", "$1"),
		sessionNameMismatch: true,
	}
	m := newTestManagerWS(f, "vb")
	_, err := m.RenameTerm("vb·term·1", "$1", testGeneration, "logs")
	if !errors.Is(err, tmux.ErrSessionIdentityChanged) {
		t.Fatalf("changed identity error = %v, want ErrSessionIdentityChanged", err)
	}
}

// TestRenameTermRefusesNonTerm pins that only sessions under the
// <ws>·term· prefix can be renamed — entry sessions (repo/worktree) are
// off-limits.
func TestRenameTermRefusesNonTerm(t *testing.T) {
	f := &fakeRunner{}
	m := newTestManagerWS(f, "vb")
	if _, err := m.RenameTerm("vb/repo1", "$1", testGeneration, "logs"); err == nil || err.Error() != "only scratch terminals can be renamed" {
		t.Fatalf("err = %v, want \"only scratch terminals can be renamed\"", err)
	}
	if strings.Contains(f.all(), "rename-session") {
		t.Errorf("rename-session should not be issued for a refused rename:\n%s", f.all())
	}
}

// TestRenameTermRefusesCollision pins that a rename that would collide
// with an existing session name is refused before tmux is asked to
// rename anything.
func TestRenameTermRefusesCollision(t *testing.T) {
	f := &fakeRunner{listOut: strings.Join([]string{
		testSessionLine("vb·term·1", "", "$1"),
		testSessionLine("vb·term·logs", "", "$2"),
	}, "\n")}
	m := newTestManagerWS(f, "vb")
	if _, err := m.RenameTerm("vb·term·1", "$1", testGeneration, "logs"); err == nil {
		t.Fatal("expected collision error")
	}
	if strings.Contains(f.all(), "rename-session") {
		t.Errorf("rename-session should not be issued on collision:\n%s", f.all())
	}
}

// TestRenameTermSanitizesLabel pins the slug sanitizer: any rune outside
// [A-Za-z0-9_-] becomes '-', with leading/trailing '-' trimmed.
func TestRenameTermSanitizesLabel(t *testing.T) {
	f := &fakeRunner{listOut: testSessionLine("vb·term·1", "", "$1")}
	m := newTestManagerWS(f, "vb")
	name, err := m.RenameTerm("vb·term·1", "$1", testGeneration, "my build!!")
	if err != nil {
		t.Fatal(err)
	}
	if name != "vb·term·my-build" {
		t.Errorf("name = %q, want vb·term·my-build", name)
	}
}

// TestRenameTermEmptyLabelErrors pins that a label which sanitizes down
// to nothing is refused.
func TestRenameTermEmptyLabelErrors(t *testing.T) {
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	if _, err := m.RenameTerm("vb·term·1", "$1", testGeneration, "!!!"); err == nil || err.Error() != "empty name" {
		t.Fatalf("err = %v, want \"empty name\"", err)
	}
}

// TestRenameTermNoop pins that renaming to the session's current name is
// a no-op: no tmux command, and the same name is returned.
func TestRenameTermNoop(t *testing.T) {
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	name, err := m.RenameTerm("vb·term·logs", "$1", testGeneration, "logs")
	if err != nil {
		t.Fatal(err)
	}
	if name != "vb·term·logs" {
		t.Errorf("name = %q, want vb·term·logs (no-op)", name)
	}
	if strings.Contains(f.all(), "rename-session") {
		t.Errorf("no-op rename should not issue rename-session:\n%s", f.all())
	}
}

// TestShutdownWorkspaceKillsOwnedSessions pins the full sweep: every
// session owned by the workspace (root, named entries, scratch and
// renamed-scratch terminals) is killed, sessions belonging to other
// workspaces (or wrap-home) are spared, the workspace's selection state
// is cleared, and the chrome kill-session is the LAST call issued.
func TestShutdownWorkspaceKillsOwnedSessions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.Write("vb", state.Selection{Session: "vb/x"}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions: map[string]bool{},
		listOut: strings.Join([]string{
			testSessionLine("vb", "", "$1"),
			testSessionLine("vb/x", "", "$2"),
			testSessionLine("vb·term·1", "", "$3"),
			testSessionLine("vb·term·logs", "", "$4"),
			testSessionLine("wrap-home", "", "$5"),
			testSessionLine("other/y", "", "$6"),
		}, "\n"),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShutdownWorkspace(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"kill-session -t $1",
		"kill-session -t $2",
		"kill-session -t $3",
		"kill-session -t $4",
	} {
		if !strings.Contains(f.all(), want) {
			t.Errorf("missing %q in:\n%s", want, f.all())
		}
	}
	for _, unwanted := range []string{
		"-L wrap kill-session -t =wrap-home",
		"-L wrap kill-session -t =other/y",
	} {
		if strings.Contains(f.all(), unwanted) {
			t.Errorf("unwanted %q in:\n%s", unwanted, f.all())
		}
	}
	if _, ok, err := state.Read("vb"); err != nil || ok {
		t.Errorf("selection state not cleared: ok=%v err=%v", ok, err)
	}
	last := f.calls[len(f.calls)-1]
	if strings.Join(last, " ") != "-f /dev/null -L wrap-ui kill-session -t =wrap-vb" {
		t.Errorf("last call = %v, want chrome kill-session", last)
	}
}

// TestShutdownWorkspaceSessionKillFailureSkipsChrome pins the error path:
// when a session in the sweep fails to die, ShutdownWorkspace returns the
// error and the chrome kill-session is NOT issued — a partial failure
// must not tear down the chrome the user needs to see the error and retry.
func TestShutdownWorkspaceSessionKillFailureSkipsChrome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions: map[string]bool{},
		listOut: strings.Join([]string{
			testSessionLine("vb", "", "$1"),
			testSessionLine("vb/x", "", "$2"),
		}, "\n"),
		failContains: "kill-session -t $2",
		failErr:      errors.New("permission denied"),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShutdownWorkspace(); err == nil {
		t.Fatal("expected error from failing session kill")
	}
	if strings.Contains(f.all(), "-f /dev/null -L wrap-ui kill-session -t =wrap-vb") {
		t.Errorf("chrome should not be killed after a sweep failure:\n%s", f.all())
	}
}

func TestShutdownWorkspaceIgnoresSessionThatExitedAfterListing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions: map[string]bool{},
		listOut:     testSessionLine("vb/x", "", "$2"),
		killErr:     "$2",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShutdownWorkspace(); err != nil {
		t.Fatalf("already-exited session blocked shutdown: %v", err)
	}
	if !strings.Contains(f.all(), "kill-session -t $2") {
		t.Fatalf("shutdown did not target stable ID:\n%s", f.all())
	}
	if !strings.Contains(f.all(), "-f /dev/null -L wrap-ui kill-session -t =wrap-vb") {
		t.Fatalf("chrome not closed after clean concurrent exit:\n%s", f.all())
	}
}

func TestShutdownWorkspaceTreatsTypedNoServerAsEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.Write("vb", state.Selection{Session: "vb/x"}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions:  map[string]bool{},
		failContains: "list-sessions",
		failErr:      errors.New("tmux list-sessions: no server running"),
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShutdownWorkspace(); err != nil {
		t.Fatalf("absent work-session server blocked chrome shutdown: %v", err)
	}
	if _, ok, err := state.Read("vb"); err != nil || ok {
		t.Fatalf("selection state not cleared: ok=%v err=%v", ok, err)
	}
	if !strings.Contains(f.all(), "-f /dev/null -L wrap-ui kill-session -t =wrap-vb") {
		t.Fatalf("chrome not closed after absent session server:\n%s", f.all())
	}
}

func TestShutdownWorkspaceRefusesReusedIDsAfterServerRestart(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		listOut:           testSessionLine("vb/x", "", "$2"),
		sessionGeneration: "fedcba9876543210fedcba9876543210",
	}
	m := newTestManagerWS(f, "vb")
	err := m.ShutdownWorkspace()
	if !errors.Is(err, tmux.ErrServerGenerationChanged) {
		t.Fatalf("ShutdownWorkspace after restart = %v, want generation change", err)
	}
	if strings.Contains(f.all(), "-L wrap-ui kill-session") {
		t.Fatalf("chrome closed after unsafe sweep:\n%s", f.all())
	}
}

func TestShutdownWorkspaceBarrierRejectsConcurrentCreation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		listStarted: make(chan struct{}),
		releaseList: make(chan struct{}),
	}
	m := newTestManagerWS(f, "vb")
	shutdownDone := make(chan error, 1)
	go func() { shutdownDone <- m.ShutdownWorkspace() }()
	<-f.listStarted

	createDone := make(chan error, 1)
	go func() {
		_, err := m.NewTerm("/workspace", "")
		createDone <- err
	}()
	close(f.releaseList)
	if err := <-shutdownDone; err != nil {
		t.Fatalf("ShutdownWorkspace: %v", err)
	}
	if err := <-createDone; err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Fatalf("concurrent creation error = %v, want shutdown barrier", err)
	}
	if strings.Contains(f.all(), "new-session -d -s vb·term") {
		t.Fatalf("concurrent terminal escaped shutdown barrier:\n%s", f.all())
	}
}

func TestCleanEnv(t *testing.T) {
	in := []string{"PATH=/bin", "TMUX=/tmp/x,1,0", "TMUX_PANE=%1", "HOME=/h"}
	out := cleanEnv(in)
	joined := strings.Join(out, " ")
	if strings.Contains(joined, "TMUX") {
		t.Errorf("TMUX leaked: %v", out)
	}
	if !strings.Contains(joined, "PATH=/bin") || !strings.Contains(joined, "HOME=/h") {
		t.Errorf("kept vars lost: %v", out)
	}
}

// A failed ClientSession read must not be treated as "not showing it".
// Failing open there silently restores the original bug: the session dies,
// no redirect happens, and tmux MRU-hops the client to whatever it likes —
// possibly another workspace's terminal.
func TestKillEntrySessionRedirectsWhenShowingCannotBeRead(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions: map[string]bool{"vb/next": true},
		paneOut:     testPanes,
		displayErr:  true, // ClientSession fails
	}
	m := newTestManagerWS(f, "vb")
	configureValidLanding(t, f, "vb/next")
	if err := m.KillEntrySession("p/e", "$9", testGeneration, "vb/next"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(f.all(), "switch-client -c /dev/ttys003 -t $7") {
		t.Errorf("unreadable client session should still redirect:\n%s", f.all())
	}
}

func TestKillEntrySessionRefusesConflictingLandingAndFallsBackHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	expected := t.TempDir()
	if err := state.WriteEntryPaths("vb", map[string]string{"vb/next": expected}); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{
		hasSessions: map[string]bool{"vb/next": true, HomeSession: true},
		paneOut:     testPanes,
		displayOut:  "p/e",
		listOut: testSessionLineAt(
			"vb/next",
			"vb/next",
			tmux.EncodeEntryPath("/stale/next"),
			"$next",
			"/stale/next",
		) + "\n" + testSessionLine(HomeSession, "", "$7"),
	}
	m := newTestManagerWS(f, "vb")
	err := m.KillEntrySession("p/e", "$9", testGeneration, "vb/next")
	if err == nil || !strings.Contains(err.Error(), "not requested path") {
		t.Fatalf("err = %v, want landing identity conflict", err)
	}
	all := f.all()
	if strings.Contains(all, "switch-client -c /dev/ttys003 -t =vb/next") {
		t.Fatalf("attached conflicting landing:\n%s", all)
	}
	if !strings.Contains(all, "switch-client -c /dev/ttys003 -t $7") {
		t.Fatalf("did not redirect to safe home fallback:\n%s", all)
	}
}

func TestKillEntrySessionLandsOnHomeAfterCandidateCheckFailure(t *testing.T) {
	for _, tc := range []struct {
		name, successor, failedTarget string
	}{
		{name: "successor check", successor: "vb/next", failedTarget: "vb/next"},
		{name: "workspace root check", failedTarget: "vb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			f := &fakeRunner{
				hasSessions:  map[string]bool{config.HomeSession: true},
				paneOut:      testPanes,
				displayOut:   "p/e",
				failContains: "has-session -t =" + tc.failedTarget,
				failErr:      errors.New("tmux permission denied"),
			}
			m := newTestManagerWS(f, "vb")
			err := m.KillEntrySession("p/e", "$9", testGeneration, tc.successor)
			if err == nil || !strings.Contains(err.Error(), "tmux permission denied") {
				t.Fatalf("err = %v, want candidate-check error after safe redirect", err)
			}
			if !strings.Contains(f.all(), "switch-client -c /dev/ttys003 -t $7") {
				t.Fatalf("candidate-check failure left client on tmux MRU:\n%s", f.all())
			}
		})
	}
}

// wrap-home is the last-resort landing and is normally guaranteed, but it
// can be killed by hand; switching a client to a session that isn't there
// just errors.
func TestLandingRecreatesHomeWhenMissing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions:      map[string]bool{}, // nothing alive, not even home
		paneOut:          testPanes,
		displayOut:       "p/e",
		displayAfterKill: "other-ws/gamma",
	}
	m := newTestManagerWS(f, "vb")
	if err := m.KillEntrySession("p/e", "$9", testGeneration, ""); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	if !strings.Contains(all, "new-session -d -s wrap-home") {
		t.Errorf("missing home session not recreated before switching to it:\n%s", all)
	}
	if !strings.Contains(all, "switch-client -c /dev/ttys003 -t $7") {
		t.Errorf("did not land on home:\n%s", all)
	}
}

func TestKillEntrySessionReportsHomeConfigurationFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	f := &fakeRunner{
		hasSessions:  map[string]bool{},
		paneOut:      testPanes,
		displayOut:   "p/e",
		failContains: "set-option -g detach-on-destroy off",
	}
	m := newTestManagerWS(f, "vb")
	err := m.KillEntrySession("p/e", "$9", testGeneration, "")
	if err == nil || !strings.Contains(err.Error(), "detach-on-destroy") {
		t.Fatalf("KillEntrySession error = %v, want fallback configuration failure", err)
	}
	if !strings.Contains(f.all(), "switch-client -c /dev/ttys003 -t $7") {
		t.Errorf("client was left on tmux's arbitrary post-kill session instead of the created fallback:\n%s", f.all())
	}
}

// The alert-bell hook is what makes bells work at all: tmux's own
// window_bell_flag is never raised on a session with a client attached,
// and the terminal pane keeps one attached to whatever it displays.
func TestEnsureSessionServerInstallsBellHook(t *testing.T) {
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	if err := m.EnsureSessionServer("/launch/dir"); err != nil {
		t.Fatal(err)
	}
	want := "-L wrap set-hook -g " + tmux.BellHook + " set-option " + tmux.BellOption + " 1"
	if !strings.Contains(f.all(), want) {
		t.Errorf("missing %q in:\n%s", want, f.all())
	}
	// The index is the whole point — see TestBellHookKeepsUserHandler.
	if !strings.Contains(tmux.BellHook, "[") {
		t.Errorf("BellHook %q is unindexed: installing it would delete the user's own alert-bell handler", tmux.BellHook)
	}
}

// The session server deliberately reads the user's tmux.conf, so wrap must
// not clobber an alert-bell hook they set there. tmux hooks are arrays: an
// unindexed `set-hook -g alert-bell` REPLACES the array, silently deleting
// theirs. Claiming one high index leaves it intact. Only real tmux knows
// that rule, so this asserts it against real tmux.
func TestBellHookKeepsUserHandler(t *testing.T) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	socket := "wrap-test-bell-hook"
	kill := func() { _ = exec.Command(bin, "-f", os.DevNull, "-L", socket, "kill-server").Run() }
	t.Cleanup(kill)
	kill()
	run := func(args ...string) string {
		out, err := exec.Command(bin, append([]string{"-f", os.DevNull, "-L", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("new-session", "-d", "-s", "t", "sleep", "30")
	// Stand in for whatever the user put in their tmux.conf.
	run("set-hook", "-g", "alert-bell", "display-message user-handler")
	run("set-hook", "-g", tmux.BellHook, "set-option "+tmux.BellOption+" 1")

	hooks := run("show-hooks", "-g")
	if !strings.Contains(hooks, "user-handler") {
		t.Errorf("wrap's hook deleted the user's alert-bell handler:\n%s", hooks)
	}
	if !strings.Contains(hooks, tmux.BellOption) {
		t.Errorf("wrap's own hook is not installed:\n%s", hooks)
	}
}

// Switching to a session is the only thing that counts as seeing it, since
// mere attachment no longer clears the flag.
func TestShowInMiddleClearsTheBell(t *testing.T) {
	f := &fakeRunner{
		hasSessions: map[string]bool{"wrap-home": true, "vb·term·1": true},
		paneOut:     testPanes,
	}
	m := newTestManagerWS(f, "vb")
	if err := m.ShowInMiddle("vb·term·1"); err != nil {
		t.Fatal(err)
	}
	want := "set-option -t $7 " + tmux.BellOption + " 0"
	if !strings.Contains(f.all(), want) {
		t.Errorf("missing %q in:\n%s", want, f.all())
	}
}

// TestIntegrationRealTmuxBellSurvivesAttachment pins the behavior that made
// bells look broken: tmux never raises window_bell_flag on a session that
// has a client attached, and wrap's terminal pane keeps one attached to
// whatever it last displayed — so the session actually running your agent
// was the one session that could never report a bell. The alert-bell hook
// fires regardless of attachment. Without it this test fails on the
// onscreen session only, which is exactly the user-visible symptom.
func TestIntegrationRealTmuxBellSurvivesAttachment(t *testing.T) {
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	sock, host := "wrap-test-e2e-bell", "wrap-test-e2e-bell-host"
	kill := func(socket string) {
		_ = exec.Command(bin, "-f", os.DevNull, "-L", socket, "kill-server").Run()
	}
	t.Cleanup(func() { kill(sock); kill(host); kill("wrap-test-e2e-bell-ui") })
	kill(sock)
	kill(host)
	run := func(socket string, args ...string) string {
		out, err := exec.Command(bin, append([]string{"-f", os.DevNull, "-L", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux -L %s %s: %v: %s", socket, strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	m := &Manager{
		UI:   &tmux.Server{Socket: "wrap-test-e2e-bell-ui", ConfigFile: os.DevNull, R: tmux.NewServer("x").R},
		Sess: &tmux.Server{Socket: sock, ConfigFile: os.DevNull, R: tmux.NewServer("x").R},
		Exe:  "/bin/wrap", WS: "ws",
	}
	if err := m.EnsureSessionServer(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"ws/onscreen", "ws/background"} {
		if err := m.Sess.NewSession(s, t.TempDir(), ""); err != nil {
			t.Fatal(err)
		}
	}

	// Attach a real client to ws/onscreen, exactly as the terminal pane does.
	// A second tmux server supplies the pty by attaching from inside one of
	// its panes — portable anywhere tmux runs, unlike script(1), whose flags
	// differ between macOS and Linux. It is also wrap's own shape: a chrome
	// server whose pane holds a client on the session server.
	run(host, "new-session", "-d", "-s", "h",
		bin+" -f "+os.DevNull+" -L "+sock+" attach -t '=ws/onscreen'")

	// The attachment IS the condition under test — an unattached ws/onscreen
	// raises window_bell_flag like any other session, so without this the
	// test would pass against the very bug it exists to catch.
	if !waitFor(t, 8*time.Second, func() bool {
		return strings.Contains(run(sock, "list-clients", "-F", "#{client_session}"), "ws/onscreen")
	}) {
		t.Fatal("no client attached to ws/onscreen — the bug this test pins cannot occur, so a pass would prove nothing")
	}

	// No "=" prefix: send-keys resolves a pane target, which rejects it
	// ("can't find pane: =ws/background"). A bare session name lands on that
	// session's active pane, and both names here are unambiguous.
	for _, s := range []string{"ws/background", "ws/onscreen"} {
		run(sock, "send-keys", "-t", s, `printf "\a"`, "Enter")
	}

	var got map[string]bool
	ok := waitFor(t, 8*time.Second, func() bool {
		infos, err := m.Sessions()
		if err != nil {
			t.Fatal(err)
		}
		got = map[string]bool{}
		for _, i := range infos {
			got[i.Name] = i.Bell
		}
		return got["ws/background"] && got["ws/onscreen"]
	})
	if !ok {
		for _, i := range mustSessions(t, m) {
			t.Logf("  %-16s attached=%v bell=%v", i.Name, i.Attached, i.Bell)
		}
		if !got["ws/background"] {
			t.Error("background session did not report a bell")
		}
		if !got["ws/onscreen"] {
			t.Error("ATTACHED session did not report a bell — this is the bug")
		}
	}
}

// waitFor polls cond until it holds or timeout elapses. Real tmux is
// asynchronous — a hook runs some time after the bell — and fixed sleeps
// either make the suite slow or make it flaky on a loaded machine.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func mustSessions(t *testing.T, m *Manager) []tmux.SessionInfo {
	t.Helper()
	infos, err := m.Sessions()
	if err != nil {
		t.Fatal(err)
	}
	return infos
}

// A raised alert must ring the terminals showing the workspace, not just
// retitle them. Terminals let a tab be renamed by hand, which pins the name
// and makes the title escape a no-op; the bell has no such override, so it
// is the half that survives.
func TestRingWorkspaceAlertRingsEveryAttachedTerminal(t *testing.T) {
	dir := t.TempDir()
	// Stand-ins for client ttys: os.OpenFile writes to these the same way.
	one, two := filepath.Join(dir, "tty1"), filepath.Join(dir, "tty2")
	for _, p := range []string{one, two} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	f := &fakeRunner{clientsOut: one + "\n" + two}
	m := newTestManagerWS(f, "vb")

	if err := m.RingWorkspaceAlert(); err != nil {
		t.Fatalf("RingWorkspaceAlert: %v", err)
	}
	for _, p := range []string{one, two} {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != "\a" {
			t.Errorf("%s got %q, want a BEL — the alert never reached this terminal", p, b)
		}
	}
	if strings.Contains(f.all(), "set-titles-string") {
		t.Errorf("one-shot ring unexpectedly changed persistent title state:\n%s", f.all())
	}
}

func TestSetWorkspaceAlertRaisesTitleWithoutRinging(t *testing.T) {
	dir := t.TempDir()
	tty := filepath.Join(dir, "tty1")
	if err := os.WriteFile(tty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{clientsOut: tty}
	m := newTestManagerWS(f, "vb")

	if err := m.SetWorkspaceAlert(true); err != nil {
		t.Fatalf("SetWorkspaceAlert: %v", err)
	}
	if !strings.Contains(f.all(), "set-titles-string 🔔 wrap: vb") {
		t.Errorf("persistent title not raised:\n%s", f.all())
	}
	b, err := os.ReadFile(tty)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Errorf("persistent title update repeated one-shot ring: got %q", b)
	}
}

// Clearing an alert retitles but must not ring: a bell means "something
// needs you", and firing one as attention is withdrawn trains the user to
// ignore it.
func TestSetWorkspaceAlertClearDoesNotRing(t *testing.T) {
	dir := t.TempDir()
	tty := filepath.Join(dir, "tty1")
	if err := os.WriteFile(tty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	f := &fakeRunner{clientsOut: tty}
	m := newTestManagerWS(f, "vb")

	if err := m.SetWorkspaceAlert(false); err != nil {
		t.Fatalf("SetWorkspaceAlert: %v", err)
	}
	b, err := os.ReadFile(tty)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 0 {
		t.Errorf("clearing an alert rang the terminal: got %q", b)
	}
}

// set-titles-string is a tmux FORMAT, and "#(...)" in a format runs a shell
// command. Workspace names are filepath.Base of a user-supplied directory,
// so an unescaped name is arbitrary command execution by directory name.
func TestWorkspaceTitleEscapesTmuxFormats(t *testing.T) {
	for _, tc := range []struct {
		ws, want string
	}{
		{"vb", "wrap: vb"},
		{"#(touch /tmp/pwned)", "wrap: ##(touch /tmp/pwned)"},
		{"a#b", "wrap: a##b"},
		{"#{session_name}", "wrap: ##{session_name}"},
	} {
		if got := workspaceTitle(tc.ws, false); got != tc.want {
			t.Errorf("workspaceTitle(%q) = %q, want %q", tc.ws, got, tc.want)
		}
	}
	if got, want := workspaceTitle("#(x)", true), "🔔 wrap: ##(x)"; got != want {
		t.Errorf("alerting title = %q, want %q", got, want)
	}
}

// The escaping above, proven against real tmux: a workspace named after a
// hostile directory must not execute when tmux renders the title. Without
// the escape this creates the marker file — verified, not assumed.
func TestWorkspaceTitleNoCommandExecutionRealTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not on PATH")
	}
	bin, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux not on PATH")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")

	// tmux expands "#()" only while a real client renders the title, so the
	// session under test needs one on a pty. A second tmux server supplies
	// it by attaching from inside one of its panes — portable anywhere tmux
	// runs, unlike script(1), whose flags differ between macOS and Linux.
	// It is also exactly wrap's own shape: chrome server, session server.
	inner, outer := "wrap-test-title-inj", "wrap-test-title-inj-host"
	kill := func(socket string) {
		_ = exec.Command(bin, "-f", os.DevNull, "-L", socket, "kill-server").Run()
	}
	t.Cleanup(func() { kill(inner); kill(outer) })
	kill(inner)
	kill(outer)

	run := func(socket string, args ...string) {
		t.Helper()
		full := append([]string{"-f", os.DevNull, "-L", socket}, args...)
		if out, err := exec.Command(bin, full...).CombinedOutput(); err != nil {
			t.Fatalf("tmux -L %s %s: %v: %s", socket, strings.Join(args, " "), err, out)
		}
	}
	run(inner, "new-session", "-d", "-s", "t", "sleep", "30")
	run(inner, "set-option", "-t", "t", "set-titles", "on")
	run(inner, "set-option", "-t", "t", "set-titles-string",
		workspaceTitle("#(touch "+marker+")", false))
	run(outer, "new-session", "-d", "-s", "host",
		bin+" -f "+os.DevNull+" -L "+inner+" attach -t t")

	// Without a client attached nothing renders and the test would pass
	// even against injectable code, so require the precondition first.
	var attached bool
	for range 40 {
		out, err := exec.Command(bin, "-f", os.DevNull, "-L", inner,
			"list-clients", "-F", "#{client_tty}").Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			attached = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !attached {
		t.Fatal("no client attached to the inner session — the title never rendered, so this test proves nothing")
	}
	// Give the format job time to run and land its side effect.
	for range 20 {
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("workspace name executed as a tmux format — the title is injectable")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// NewTerm creates a session directly, so it has to configure the server
// first — exactly as EnsureEntrySession already does.
//
// The session server can be gone while a workspace is still open: a
// reboot, a crash, a stray `tmux -L wrap kill-server`. Sessions() reports
// that as an empty list rather than an error, so NewTerm sailed past it
// and let tmux auto-start a bare server for its new-session. That server
// has none of wrap's settings — no detach-on-destroy, no monitor-bell,
// and no alert-bell hook — so every terminal made afterward silently
// stopped ringing, with nothing on screen to say why.
func TestNewTermConfiguresTheSessionServer(t *testing.T) {
	f := &fakeRunner{hasSessions: map[string]bool{}}
	m := newTestManagerWS(f, "vb")
	if _, err := m.NewTerm("/dir", ""); err != nil {
		t.Fatal(err)
	}
	all := f.all()
	for _, want := range []string{
		"set-hook -g " + tmux.BellHook + " set-option " + tmux.BellOption + " 1",
		"set-option -wg monitor-bell on",
		"set-option -g detach-on-destroy off",
		"set-option -g set-clipboard on",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q — a terminal made on a fresh server loses it:\n%s", want, all)
		}
	}
	// Ordering is the point: settings applied after the fact would leave a
	// window between server start and configuration.
	if hook, sess := strings.Index(all, "set-hook"), strings.Index(all, "new-session -d -P -F '#{session_id}' -s vb·term·1"); hook < 0 || sess < 0 || hook > sess {
		t.Errorf("server configured after the terminal was created (hook@%d, new-session@%d):\n%s", hook, sess, all)
	}
}

// TestRingTTYDoesNotBlockOnAStalledDevice is the regression for a bell
// write that could wedge the terminals pane. ringOuterTerminal runs
// synchronously inside that pane's Update, so a terminal which has
// stopped reading its output must return an error rather than park the
// caller until it drains — which, for a stopped emulator, is never.
//
// A FIFO stands in for the tty: both are character-ish devices with a
// bounded kernel buffer and a reader that can simply stop, and unlike a
// pty a FIFO can be stalled deterministically by filling it.
func TestRingTTYDoesNotBlockOnAStalledDevice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	// A reader must exist or the write side gets ENXIO instead of the
	// backpressure this test is about. It is opened and then deliberately
	// never read from: that is the stalled terminal.
	rfd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open read end: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Close(rfd); err != nil {
			t.Errorf("close read end: %v", err)
		}
	})

	wfd, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("open write end: %v", err)
	}
	t.Cleanup(func() {
		if err := syscall.Close(wfd); err != nil {
			t.Errorf("close write end: %v", err)
		}
	})
	blob := make([]byte, 1<<16)
	for i := 0; i < 64; i++ {
		if _, err := syscall.Write(wfd, blob); err == syscall.EAGAIN {
			break
		} else if err != nil {
			t.Fatalf("filling fifo: %v", err)
		}
		if i == 63 {
			t.Fatal("fifo never filled; it cannot exert backpressure here")
		}
	}

	done := make(chan error, 1)
	go func() { done <- ringTTY(path) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ringTTY reported success writing to a device that cannot accept the byte")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ringTTY blocked on a stalled device; it must fail fast instead")
	}
}
