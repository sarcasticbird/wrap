package tree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/gitx"
	"github.com/sarcasticbird/wrap/internal/state"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

type fakeBackend struct {
	sessionsErr      error
	killSuccessor    string
	killID           string
	killGeneration   string
	switched, killed []string
	detached         bool
	sessions         []tmux.SessionInfo
	shutdownCalls    int
	shutdownErr      error
	diffCalls        []diffCall
	diffErr          error
	selectionErr     error
	selections       []state.Selection
}

func (f *fakeBackend) WriteSelection(ws string, sel state.Selection) error {
	f.selections = append(f.selections, sel)
	if f.selectionErr != nil {
		return f.selectionErr
	}
	return state.Write(ws, sel)
}

// diffCall pins one ShowDiff invocation's arguments for assertion.
type diffCall struct {
	repoRoot, relPath string
	staged, untracked bool
}

func (f *fakeBackend) SwitchMiddle(target string) error {
	f.switched = append(f.switched, target)
	return nil
}
func (f *fakeBackend) KillEntrySession(name, targetID, targetGeneration, successor string) error {
	f.killSuccessor = successor
	f.killID = targetID
	f.killGeneration = targetGeneration
	f.killed = append(f.killed, name)
	return nil
}
func (f *fakeBackend) Sessions() ([]tmux.SessionInfo, error) {
	if f.sessionsErr != nil {
		return nil, f.sessionsErr
	}
	return f.sessions, nil
}
func (f *fakeBackend) DetachUI() error { f.detached = true; return nil }
func (f *fakeBackend) ShutdownWorkspace() error {
	f.shutdownCalls++
	return f.shutdownErr
}
func (f *fakeBackend) ShowDiff(repoRoot, relPath string, staged, untracked bool) error {
	f.diffCalls = append(f.diffCalls, diffCall{repoRoot, relPath, staged, untracked})
	return f.diffErr
}

func key(s string) tea.KeyMsg {
	if s == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func oneRepoOpts(b Backend) Options {
	return Options{
		WS: "ws", Root: "/root", RootName: "root",
		Repos: []gitx.Discovered{{Name: "repo1", Path: "/r1", Kind: gitx.DiscoveredRepo}},
	}
}

func TestViewHasGitHeadingAndCompactFooter(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{
		WS: "ws", Root: "/root", RootName: "root",
		Keys: config.Keys{FocusTree: "M-2"},
	})
	view := ansi.Strip(m.View())
	for _, want := range []string{"Git (⌥2)", "h help · q detach · Q shutdown"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
}

func TestLeftRightAreOnlyGitExpansionKeys(t *testing.T) {
	m := NewModel(&fakeBackend{}, oneRepoOpts(&fakeBackend{}))
	m.git["/r1"] = repoGit{branch: "main", dirty: 1}
	m.Cursor = 1

	mod, _ := m.Update(tea.KeyMsg{Type: tea.KeyRight})
	if !mod.(Model).expanded["/r1"] {
		t.Fatal("Right did not expand repo")
	}
	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if mod.(Model).expanded["/r1"] {
		t.Fatal("Left did not collapse repo")
	}
	mod, _ = mod.Update(key("l"))
	if mod.(Model).expanded["/r1"] {
		t.Fatal("l remains an expansion alias")
	}
}

func TestGitHelpCloseKeysDoNotDetach(t *testing.T) {
	for _, closeKey := range []tea.KeyMsg{
		key("h"),
		{Type: tea.KeyEsc},
		key("q"),
	} {
		t.Run(closeKey.String(), func(t *testing.T) {
			b := &fakeBackend{}
			m := NewModel(b, Options{WS: "ws", Root: "/root", RootName: "root"})
			mod, _ := m.Update(key("h"))
			if !mod.(Model).helpOpen {
				t.Fatal("h did not open Help")
			}
			mod, _ = mod.Update(closeKey)
			if mod.(Model).helpOpen {
				t.Fatalf("%q did not close Help", closeKey.String())
			}
			if b.detached {
				t.Fatalf("%q detached while closing Help", closeKey.String())
			}
		})
	}
}

func TestGitHelpKeepsActionsInertAndPollingLive(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, oneRepoOpts(b))
	m.Cursor = 1
	m.expanded["/r1"] = true
	mod, _ := m.Update(key("h"))

	mod, _ = mod.Update(key("j"))
	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 0})
	mod, _ = mod.Update(key("Q"))
	got := mod.(Model)
	if got.Cursor != 1 || !got.expanded["/r1"] || got.ConfirmShutdown {
		t.Fatalf("Help mutated actions: cursor=%d expanded=%v shutdown=%v", got.Cursor, got.expanded["/r1"], got.ConfirmShutdown)
	}

	mod, _ = mod.Update(sessionsMsg{sessions: []tmux.SessionInfo{{Name: "ws/repo1"}}})
	got = mod.(Model)
	if _, ok := got.sessions["ws/repo1"]; !ok {
		t.Fatal("session poll was ignored while Help was open")
	}
}

func TestGitHelpFitsSmallPaneHeights(t *testing.T) {
	for _, height := range []int{1, 2} {
		t.Run(fmt.Sprintf("height_%d", height), func(t *testing.T) {
			m := NewModel(&fakeBackend{}, Options{WS: "ws", Root: "/root", RootName: "root"})
			m.Width = 80
			m.Height = height
			m.helpOpen = true

			lines := strings.Split(ansi.Strip(m.View()), "\n")
			if len(lines) > height {
				t.Fatalf("Help rendered %d lines into height %d:\n%s", len(lines), height, m.View())
			}
			if height == 2 && !strings.Contains(lines[1], "close") {
				t.Fatalf("two-line Help should retain close hint:\n%s", m.View())
			}
		})
	}
}

func TestGitConfirmationKeepsPriorityOverHelp(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "ws", Root: "/root", RootName: "root"})
	mod, _ := m.Update(key("Q"))
	mod, _ = mod.Update(key("h"))
	got := mod.(Model)
	if got.ConfirmShutdown || got.helpOpen {
		t.Fatalf("h should cancel confirmation without opening Help: %+v", got.Nav)
	}
}

func TestFetchSessionsSurfacesMalformedSelection(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	dir := filepath.Join(stateHome, "wrap", "vb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: t.TempDir()})
	msg := m.fetchSessions()().(sessionsMsg)
	if msg.selectionErr == nil || !strings.Contains(msg.selectionErr.Error(), "unmarshal") {
		t.Fatalf("selection error = %v, want malformed state error", msg.selectionErr)
	}
}

func TestMalformedSelectionDoesNotFreezeTreeSessions(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	dir := filepath.Join(stateHome, "wrap", "vb")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{sessions: []tmux.SessionInfo{{Name: "vb/api", Bell: true}}}
	m := NewModel(b, Options{WS: "vb", Root: t.TempDir()})

	updated, _ := m.Update(m.fetchSessions()())
	got := updated.(Model)
	if info, ok := got.sessions["vb/api"]; !ok || !info.Bell {
		t.Fatalf("sessions = %+v, want fresh ringing vb/api despite malformed selection", got.sessions)
	}
	if footer := got.footer(); !strings.Contains(footer, "unmarshal") {
		t.Fatalf("footer = %q, want malformed selection error", footer)
	}
}

func TestSessionPollSchedulesNextOnlyAfterCompletion(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := NewModel(&fakeBackend{}, Options{WS: "ws", Root: t.TempDir()})
	mod, cmd := m.Update(sessionsTickMsg{})
	if cmd == nil {
		t.Fatal("tick did not start a session poll")
	}
	msg := cmd()
	if _, ok := msg.(sessionsMsg); !ok {
		t.Fatalf("tick command returned %T, want one sessionsMsg (no overlapping timer batch)", msg)
	}
	_, next := mod.Update(msg)
	if next == nil {
		t.Fatal("completed session poll did not schedule the next tick")
	}
}

func TestGitPollSchedulesNextOnlyAfterCompletion(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{
		WS: "ws", Root: t.TempDir(),
		Status: func(string) (*gitx.Snapshot, error) {
			return nil, gitx.ErrNotARepo
		},
	})
	mod, cmd := m.Update(gitTickMsg{})
	if cmd == nil {
		t.Fatal("tick did not start a Git poll")
	}
	msg := cmd()
	if _, ok := msg.(gitMsg); !ok {
		t.Fatalf("tick command returned %T, want one gitMsg (no overlapping timer batch)", msg)
	}
	_, next := mod.Update(msg)
	if next == nil {
		t.Fatal("completed Git poll did not schedule the next tick")
	}
}

func TestManualGitRefreshDoesNotCreateSecondTimerChain(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "ws", Root: t.TempDir()})
	m.gitPolling = true
	m.gitTimerPending = true
	mod, next := m.Update(gitMsg{})
	got := mod.(Model)
	if next != nil {
		t.Fatal("manual Git refresh scheduled a timer while the regular timer was still pending")
	}
	if !got.gitTimerPending || got.gitPolling {
		t.Fatalf("timer/poll state = pending:%v polling:%v", got.gitTimerPending, got.gitPolling)
	}
}

func TestRowsNoHeadingRootFirst(t *testing.T) {
	m := NewModel(&fakeBackend{}, oneRepoOpts(&fakeBackend{}))
	rows := m.rows()
	if len(rows) != 2 {
		t.Fatalf("rows = %+v", rows)
	}
	if rows[0].kind != rowRoot || rows[0].name != "root" || rows[0].session != "ws" {
		t.Errorf("root row = %+v", rows[0])
	}
	if v := m.View(); strings.Contains(v, " wrap ") {
		t.Errorf("unexpected sidebar-style title in view:\n%s", v)
	}
}

func TestRootRepositoryGitErrorIsVisible(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "ws", Root: "/root", RootName: "root"})
	updated, _ := m.Update(gitMsg{"/root": {err: "permission denied"}})
	view := updated.(Model).View()
	if !strings.Contains(view, "⚠") {
		t.Fatalf("view = %q, want root repository Git warning", view)
	}
}

func TestUmbrellaRootNotRepositoryHasNoWarning(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{
		WS: "ws", Root: "/root", RootName: "root",
		Status: func(string) (*gitx.Snapshot, error) { return nil, gitx.ErrNotARepo },
		GitMetadata: func(string) (bool, error) {
			return false, nil
		},
	})
	updated, _ := m.Update(m.fetchGit()())
	view := updated.(Model).View()
	if strings.Contains(view, "⚠") {
		t.Fatalf("view = %q, ordinary non-repository workspace root should not warn", view)
	}
}

func TestRootErrNotARepoWithAncestorGitMetadataIsVisible(t *testing.T) {
	parent := t.TempDir()
	if err := os.Mkdir(filepath.Join(parent, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "workspace")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	m := NewModel(&fakeBackend{}, Options{
		WS: "ws", Root: root, RootName: "root",
		Status: gitx.Status,
	})
	updated, _ := m.Update(m.fetchGit()())
	view := updated.(Model).View()
	if !strings.Contains(view, "⚠") {
		t.Fatalf("view = %q, invalid repository metadata must warn", view)
	}
}

func TestExpandedRootTakeErrorIsVisible(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{
		WS: "ws", Root: "/root", RootName: "root",
		Status: func(string) (*gitx.Snapshot, error) {
			return &gitx.Snapshot{Branch: "main"}, nil
		},
		Take: func(string) (*gitx.Snapshot, error) {
			return nil, errors.New("cannot read object")
		},
	})
	m.expanded["/root"] = true
	updated, _ := m.Update(m.fetchGit()())
	view := updated.(Model).View()
	if !strings.Contains(view, "⎇main") || !strings.Contains(view, "⚠") {
		t.Fatalf("view = %q, want branch and detailed-query warning", view)
	}
}

func TestGitRefreshClampsCursorAfterRowsShrink(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, oneRepoOpts(b))
	m.expanded["/r1"] = true
	m.git["/r1"] = repoGit{snap: &gitx.Snapshot{
		Untracked: []string{"a", "b", "c"},
	}}
	m.Cursor = len(m.rows()) - 1

	mod, _ := m.Update(gitMsg{"/r1": {snap: &gitx.Snapshot{}}})
	got := mod.(Model)
	if got.Cursor != len(got.rows())-1 {
		t.Fatalf("cursor = %d with %d rows after refresh; want clamped to last row", got.Cursor, len(got.rows()))
	}
}

func TestRepoRowShowsBranchAndDirty(t *testing.T) {
	m := NewModel(&fakeBackend{}, oneRepoOpts(&fakeBackend{}))
	var mod tea.Model = m
	mod, _ = mod.Update(gitMsg{"/r1": {branch: "main", dirty: 3}})
	v := mod.View()
	if !strings.Contains(v, "⎇main") {
		t.Errorf("missing branch in view:\n%s", v)
	}
	if !strings.Contains(v, "[3]") {
		t.Errorf("missing dirty count in view:\n%s", v)
	}
}

func TestExpandShowsFiles(t *testing.T) {
	statusFn := func(string) (*gitx.Snapshot, error) { return &gitx.Snapshot{Branch: "main"}, nil }
	takeFn := func(string) (*gitx.Snapshot, error) {
		return &gitx.Snapshot{
			Branch:    "main",
			Staged:    []gitx.FileChange{{Path: "a.go", Status: 'M', Added: 42, Deleted: 7}},
			Unstaged:  []gitx.FileChange{{Path: "b.go", Status: 'M', Added: 1, Deleted: 2}},
			Untracked: []string{"c.txt"},
		}, nil
	}
	o := oneRepoOpts(nil)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(&fakeBackend{}, o)
	var mod tea.Model = m
	mod, _ = mod.Update(key("j")) // cursor: root -> repo1

	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // expand
	if cmd == nil {
		t.Fatal("expanding should return an immediate git refresh cmd")
	}
	mod, _ = mod.Update(cmd())

	sm := mod.(Model)
	rows := sm.rows()
	if len(rows) != 5 { // root, repo1, staged, unstaged, untracked
		t.Fatalf("rows after expand = %+v", rows)
	}
	if v := sm.View(); !strings.Contains(v, "+42") {
		t.Errorf("missing staged counts in view:\n%s", v)
	}

	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyLeft}) // collapse
	sm = mod.(Model)
	if got := len(sm.rows()); got != 2 {
		t.Errorf("collapsed rows = %d, want 2: %+v", got, sm.rows())
	}
	if v := sm.View(); strings.Contains(v, "+42") {
		t.Errorf("collapsed view should not show file rows:\n%s", v)
	}
}

// TestEnterSelectsAndWritesState pins that the tree NEVER creates a
// terminal session: Enter on a row whose session doesn't exist yet still
// writes the selection to state (so `n` in terms knows what to bind) but
// does not switch the middle pane, and surfaces a dim info message
// telling the user how to get a terminal.
func TestEnterSelectsAndWritesState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := &fakeBackend{}
	o := oneRepoOpts(b)
	o.Cmd = "vim"
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(key("j")) // cursor -> repo1
	mod, _ = mod.Update(key("enter"))

	want := config.SessionName("ws", "repo1")
	if len(b.switched) != 0 {
		t.Fatalf("switched = %v, want none (no session exists yet)", b.switched)
	}
	sel, ok, err := state.Read("ws")
	if err != nil || !ok || sel.Session != want || sel.Entry != "repo1" || sel.Path != "/r1" {
		t.Fatalf("state = %+v ok=%v err=%v", sel, ok, err)
	}
	if v := mod.View(); !strings.Contains(v, "no terminal — press n in terminals") {
		t.Errorf("expected session-less info message in view:\n%s", v)
	}
	_ = mod
}

// TestEnterSwitchesLiveSession pins the complementary case: Enter on a
// row whose session DOES exist (seeded via sessionsMsg, as the 2s tick
// would) switches the middle pane and never shows the session-less info
// message. It does not create anything.
func TestEnterSwitchesLiveSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := &fakeBackend{}
	o := oneRepoOpts(b)
	m := NewModel(b, o)
	want := config.SessionName("ws", "repo1")
	var mod tea.Model = m
	mod, _ = mod.Update(sessionsMsg{sessions: []tmux.SessionInfo{{Name: want}}})
	mod, _ = mod.Update(key("j")) // cursor -> repo1
	mod, _ = mod.Update(key("enter"))

	if len(b.switched) != 1 || b.switched[0] != want {
		t.Fatalf("switched = %v", b.switched)
	}
	sel, ok, err := state.Read("ws")
	if err != nil || !ok || sel.Session != want {
		t.Fatalf("state = %+v ok=%v err=%v", sel, ok, err)
	}
	if v := mod.View(); strings.Contains(v, "no terminal") {
		t.Errorf("live session should not show session-less info message, view:\n%s", v)
	}
}

func TestRootRowSelectable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "ws", Root: "/root", RootName: "root", Cmd: "zsh"})
	var mod tea.Model = m
	mod, _ = mod.Update(sessionsMsg{sessions: []tmux.SessionInfo{{Name: "ws"}}})
	mod, _ = mod.Update(key("enter"))
	if len(b.switched) != 1 || b.switched[0] != "ws" {
		t.Fatalf("root enter should select ws session: switched=%v", b.switched)
	}
	sel, ok, err := state.Read("ws")
	if err != nil || !ok || sel.Session != "ws" || sel.Entry != "root" || sel.Path != "/root" {
		t.Fatalf("state = %+v ok=%v err=%v", sel, ok, err)
	}

	// The heading occupies line 0, so the root row begins at line 1.
	// A click only moves the cursor; Enter is what reaches the backend.
	b2 := &fakeBackend{}
	m2 := NewModel(b2, Options{WS: "ws2", Root: "/root2", RootName: "root2"})
	var mod2 tea.Model = m2
	mod2, _ = mod2.Update(sessionsMsg{sessions: []tmux.SessionInfo{{Name: "ws2"}}})
	mod2, _ = mod2.Update(tea.MouseMsg{Y: 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if len(b2.switched) != 0 {
		t.Fatalf("click alone should not activate: switched=%v", b2.switched)
	}
	if sm2 := mod2.(Model); sm2.Cursor != 0 {
		t.Fatalf("click at Y=1 should set cursor to 0, got %d", sm2.Cursor)
	}
	mod2.Update(key("enter"))
	if len(b2.switched) != 1 || b2.switched[0] != "ws2" {
		t.Fatalf("Enter after click-select should activate: switched=%v", b2.switched)
	}
	_ = mod
}

// TestMouseClickMovesCursorNoBackendCalls pins the commit-2 fix: a left
// click on an activatable row only moves the cursor — it never calls the
// backend. Confirmed against a row (repo1) that WOULD activate via Enter.
func TestMouseClickMovesCursorNoBackendCalls(t *testing.T) {
	session := config.SessionName("ws", "repo1")
	b := &fakeBackend{sessions: []tmux.SessionInfo{{ID: "$7", Generation: "generation", Name: session}}}
	m := NewModel(b, oneRepoOpts(b))
	var mod tea.Model = m
	mod, _ = mod.Update(sessionsMsg{sessions: b.sessions})
	// repo1 is model row 1 and physical line 2 after the heading.
	mod, _ = mod.Update(tea.MouseMsg{Y: 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	sm := mod.(Model)
	if sm.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1", sm.Cursor)
	}
	if len(b.switched) != 0 || len(b.killed) != 0 {
		t.Fatalf("click should make zero backend calls: switched=%v killed=%v", b.switched, b.killed)
	}
}

// TestMouseWheelMovesCursor pins the wheel-scroll fix: wheel up/down steps
// the cursor by one row, clamped at both ends. bubbletea v1.3.10 reports
// wheel events with Action == MouseActionPress (its zero value) rather
// than a dedicated wheel action — confirmed in the vendored mouse.go
// source, where the motion bit and release-downgrade are both explicitly
// skipped for wheel buttons — so dispatch here is on Button alone.
func TestMouseWheelMovesCursor(t *testing.T) {
	o := Options{
		WS: "ws", Root: "/root", RootName: "root",
		Repos: []gitx.Discovered{
			{Name: "repo0", Path: "/r0", Kind: gitx.DiscoveredRepo},
			{Name: "repo1", Path: "/r1", Kind: gitx.DiscoveredRepo},
		},
	}
	b := &fakeBackend{}
	m := NewModel(b, o) // rows: root(0), repo0(1), repo1(2)
	var mod tea.Model = m

	// Wheel up at the top clamps to 0.
	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if sm := mod.(Model); sm.Cursor != 0 {
		t.Fatalf("wheel up at top should clamp to 0, got %d", sm.Cursor)
	}

	// Wheel down moves the cursor forward one row at a time.
	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if sm := mod.(Model); sm.Cursor != 1 {
		t.Fatalf("wheel down should move cursor to 1, got %d", sm.Cursor)
	}
	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if sm := mod.(Model); sm.Cursor != 2 {
		t.Fatalf("wheel down should move cursor to 2, got %d", sm.Cursor)
	}
	// Wheel down at the bottom clamps to the last row.
	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if sm := mod.(Model); sm.Cursor != 2 {
		t.Fatalf("wheel down at bottom should clamp to last row (2), got %d", sm.Cursor)
	}
	if len(b.switched) != 0 || len(b.killed) != 0 {
		t.Fatalf("wheel should make zero backend calls: switched=%v killed=%v", b.switched, b.killed)
	}

	// Wheel up moves back up.
	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if sm := mod.(Model); sm.Cursor != 1 {
		t.Fatalf("wheel up should move cursor to 1, got %d", sm.Cursor)
	}
}

func TestViewKeepsCursorAndKillTargetInsideViewport(t *testing.T) {
	repos := make([]gitx.Discovered, 10)
	for i := range repos {
		repos[i] = gitx.Discovered{
			Name: fmt.Sprintf("repo-%02d", i),
			Path: fmt.Sprintf("/repo-%02d", i),
			Kind: gitx.DiscoveredRepo,
		}
	}
	m := NewModel(&fakeBackend{}, Options{WS: "ws", Root: "/root", RootName: "root", Repos: repos})
	m.Height = 5
	m.Cursor = len(m.rows()) - 1
	if view := m.View(); !strings.Contains(view, "repo-09") || strings.Contains(view, "repo-00") {
		t.Fatalf("viewport did not keep the last cursor visible:\n%s", view)
	}

	m.ArmKill(config.SessionName("ws", "repo-00"), "$7", "generation", "")
	if view := m.View(); !strings.Contains(view, "repo-00") {
		t.Fatalf("viewport did not keep the confirmation target visible:\n%s", view)
	}
}

func TestViewportTranslatesMouseRows(t *testing.T) {
	repos := make([]gitx.Discovered, 10)
	for i := range repos {
		repos[i] = gitx.Discovered{Name: fmt.Sprintf("repo-%02d", i), Path: fmt.Sprintf("/repo-%02d", i)}
	}
	m := NewModel(&fakeBackend{}, Options{WS: "ws", Root: "/root", RootName: "root", Repos: repos})
	m.Height = 5
	m.Cursor = len(m.rows()) - 1
	mod, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 1})
	if got := mod.(Model).Cursor; got != len(m.rows())-3 {
		t.Fatalf("cursor = %d, want first visible row %d", got, len(m.rows())-3)
	}
}

func TestViewportIgnoresClicksBelowVisibleRows(t *testing.T) {
	repos := make([]gitx.Discovered, 10)
	for i := range repos {
		repos[i] = gitx.Discovered{Name: fmt.Sprintf("repo-%02d", i), Path: fmt.Sprintf("/repo-%02d", i)}
	}
	m := NewModel(&fakeBackend{}, Options{WS: "ws", Root: "/root", RootName: "root", Repos: repos})
	m.Height = 5
	m.Cursor = len(m.rows()) - 1
	before := m.Cursor
	mod, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 4})
	if got := mod.(Model).Cursor; got != before {
		t.Fatalf("footer click moved cursor from %d to hidden row %d", before, got)
	}
}

func TestWorktreeKidsListed(t *testing.T) {
	wt := func(dir string) ([]gitx.Worktree, error) {
		return []gitx.Worktree{{Path: "/wt-a", Branch: "feature"}}, nil
	}
	o := oneRepoOpts(nil)
	o.Worktrees = wt
	m := NewModel(&fakeBackend{}, o)
	rows := m.rows()
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	want := config.SessionName("ws", "repo1/wt-a")
	if rows[2].kind != rowRepo || rows[2].name != "repo1/wt-a" || rows[2].path != "/wt-a" || rows[2].session != want {
		t.Errorf("worktree row = %+v", rows[2])
	}
}

// TestExpandAffordance pins the l/h expand affordance: a dirty repo row
// shows ▸ while collapsed, ▾ once expanded (with its file rows visible),
// and back to ▸ with file rows gone once collapsed again.
func TestExpandAffordance(t *testing.T) {
	// Path-aware: fetchGit now also statuses the workspace root
	// alongside repo1, and this test's root is a plain umbrella (no
	// branch) — only repo1 should come back dirty.
	statusFn := func(dir string) (*gitx.Snapshot, error) {
		if dir != "/r1" {
			return &gitx.Snapshot{}, nil
		}
		return &gitx.Snapshot{Branch: "main", Untracked: []string{"c.txt"}}, nil
	}
	takeFn := func(dir string) (*gitx.Snapshot, error) {
		if dir != "/r1" {
			return &gitx.Snapshot{}, nil
		}
		return &gitx.Snapshot{Branch: "main", Untracked: []string{"c.txt"}}, nil
	}
	o := oneRepoOpts(nil)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(&fakeBackend{}, o)
	var mod tea.Model = m

	// A poll found the repo dirty; still collapsed.
	mod, _ = mod.Update(gitMsg{"/r1": {branch: "main", dirty: 1}})
	if v := mod.View(); !strings.Contains(v, "▸ ") {
		t.Errorf("collapsed dirty repo should show ▸, view:\n%s", v)
	}
	if v := mod.View(); strings.Contains(v, "▾") {
		t.Errorf("collapsed dirty repo should not show ▾, view:\n%s", v)
	}

	mod, _ = mod.Update(key("j")) // cursor -> repo1
	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // expand
	mod, _ = mod.Update(cmd())

	sm := mod.(Model)
	v := sm.View()
	if !strings.Contains(v, "▾ ") {
		t.Errorf("expanded dirty repo should show ▾, view:\n%s", v)
	}
	if strings.Contains(v, "▸") {
		t.Errorf("expanded dirty repo should not show ▸, view:\n%s", v)
	}
	if !strings.Contains(v, "c.txt") {
		t.Errorf("expanded repo should show its file rows, view:\n%s", v)
	}

	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyLeft}) // collapse
	sm = mod.(Model)
	v = sm.View()
	if !strings.Contains(v, "▸ ") {
		t.Errorf("collapsed dirty repo should show ▸ again, view:\n%s", v)
	}
	if strings.Contains(v, "c.txt") {
		t.Errorf("collapsed repo should not show file rows, view:\n%s", v)
	}
}

// TestCleanRepoNoArrow pins that a repo with no changes shows neither
// arrow — just the alignment-preserving two-space prefix.
func TestCleanRepoNoArrow(t *testing.T) {
	m := NewModel(&fakeBackend{}, oneRepoOpts(&fakeBackend{}))
	var mod tea.Model = m
	mod, _ = mod.Update(gitMsg{"/r1": {branch: "main", dirty: 0}})
	v := mod.View()
	if strings.Contains(v, "▸") || strings.Contains(v, "▾") {
		t.Errorf("clean repo should show neither arrow, view:\n%s", v)
	}
}

func TestKillConfirmFlow(t *testing.T) {
	session := config.SessionName("ws", "repo1")
	b := &fakeBackend{sessions: []tmux.SessionInfo{{ID: "$7", Generation: "generation", Name: session}}}
	m := NewModel(b, oneRepoOpts(b))
	var mod tea.Model = m
	mod, _ = mod.Update(sessionsMsg{sessions: b.sessions})
	mod, _ = mod.Update(key("j")) // cursor -> repo1
	mod, _ = mod.Update(key("x")) // ask for confirmation
	if len(b.killed) != 0 {
		t.Fatal("killed before confirm")
	}
	if v := mod.View(); !strings.Contains(v, "kill "+session+"? y/n") {
		t.Errorf("confirm prompt should name target %q, view:\n%s", session, v)
	}
	mod.Update(key("y"))
	if len(b.killed) != 1 || b.killed[0] != session {
		t.Errorf("killed = %v", b.killed)
	}
	if b.killID != "$7" {
		t.Errorf("stable kill ID = %q, want $7", b.killID)
	}
	if b.killGeneration != "generation" {
		t.Errorf("kill generation = %q, want generation", b.killGeneration)
	}
}

// TestKillConfirmSurvivesRowChurn pins I2: the session captured at "x"
// time is the one killed on "y", even if the row layout changes in
// between (e.g. a repo expands and shifts what sits at the old cursor
// index). Killing by stale row index would kill the wrong target — or,
// once a file row lands there, an empty session name.
func TestKillConfirmSurvivesRowChurn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo0Session := config.SessionName("ws", "repo0")
	repo1Session := config.SessionName("ws", "repo1")
	o := Options{
		WS: "ws", Root: "/root", RootName: "root",
		Repos: []gitx.Discovered{
			{Name: "repo0", Path: "/r0", Kind: gitx.DiscoveredRepo},
			{Name: "repo1", Path: "/r1", Kind: gitx.DiscoveredRepo},
		},
	}
	b := &fakeBackend{sessions: []tmux.SessionInfo{
		{ID: "$0", Generation: "generation", Name: repo0Session},
		{ID: "$1", Generation: "generation", Name: repo1Session},
	}}
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(sessionsMsg{sessions: b.sessions})

	// Expand repo0 with one file so rows = [root, repo0, file, repo1].
	mod, _ = mod.Update(key("j")) // cursor -> repo0
	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight})
	mod, _ = mod.Update(cmd())
	mod, _ = mod.Update(gitMsg{"/r0": {snap: &gitx.Snapshot{Untracked: []string{"f1.txt"}}}})

	// Move cursor onto repo1 (now at index 3) and open the confirm prompt.
	mod, _ = mod.Update(key("j")) // -> file row
	mod, _ = mod.Update(key("j")) // -> repo1
	sm := mod.(Model)
	if rows := sm.rows(); rows[sm.Cursor].session != repo1Session {
		t.Fatalf("setup: cursor should be on repo1, rows=%+v cursor=%d", rows, sm.Cursor)
	}
	mod, _ = mod.Update(key("x")) // confirm — captures repo1Session

	// Rows grow: repo0 gets a second untracked file, pushing repo1 one
	// row further down and putting a FILE row at the old cursor index.
	mod, _ = mod.Update(gitMsg{"/r0": {snap: &gitx.Snapshot{Untracked: []string{"f1.txt", "f2.txt"}}}})
	sm = mod.(Model)
	if rows := sm.rows(); rows[sm.Cursor].kind != rowFile {
		t.Fatalf("setup: growth should leave a file row at the old cursor index, rows=%+v cursor=%d", rows, sm.Cursor)
	}

	mod.Update(key("y"))
	if len(b.killed) != 1 || b.killed[0] != repo1Session {
		t.Errorf("killed = %v, want [%s] (the session captured at x-time)", b.killed, repo1Session)
	}
	if b.killID != "$1" {
		t.Errorf("kill ID = %q, want original $1", b.killID)
	}
	if b.killGeneration != "generation" {
		t.Errorf("kill generation = %q, want original generation", b.killGeneration)
	}
}

// TestSessionsMsgCurrentFromState pins I4: the tree learns the current
// selection from a successful state read (not just its own selectRow),
// so a session that's actually on-screen never falsely shows "!" even
// when its activity has moved past the last-seen baseline (e.g. the
// user is typing in it).
func TestSessionsMsgCurrentFromState(t *testing.T) {
	session := config.SessionName("ws", "repo1")
	b := &fakeBackend{}
	m := NewModel(b, oneRepoOpts(b))
	var mod tea.Model = m
	// First sighting baselines lastSeen; state already says this
	// session is current (e.g. selected via a prior process/pane).
	mod, _ = mod.Update(sessionsMsg{sessions: []tmux.SessionInfo{{Name: session, Activity: 100}}, current: session, ok: true})
	// Activity moves past the baseline while state still says current —
	// the typing-session false-alert scenario.
	mod, _ = mod.Update(sessionsMsg{sessions: []tmux.SessionInfo{{Name: session, Activity: 200}}, current: session, ok: true})
	v := mod.View()
	if strings.Contains(v, "!") {
		t.Errorf("current session (learned from state) falsely flagged as new activity:\n%s", v)
	}
}

type deadSessionBackend struct {
	fakeBackend
}

func (d *deadSessionBackend) SwitchMiddle(target string) error {
	return fmt.Errorf("tmux switch-client: can't find session %s", target)
}

func TestDeadSessionHumanError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	session := config.SessionName("ws", "repo1")
	b := &deadSessionBackend{}
	m := NewModel(b, oneRepoOpts(b))
	var mod tea.Model = m
	// The session existed at the last tick (so selectRow attempts the
	// switch), but a fresh Sessions() call — made after SwitchMiddle
	// fails — comes back without it: it died between tick and switch.
	mod, _ = mod.Update(sessionsMsg{sessions: []tmux.SessionInfo{{Name: session}}})
	mod, _ = mod.Update(key("j"))
	mod, _ = mod.Update(key("enter"))
	if v := mod.View(); !strings.Contains(v, "exited immediately") {
		t.Errorf("expected human dead-session error, got:\n%s", v)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("short.go", 40); got != "short.go" {
		t.Errorf("fitting string should be unchanged, got %q", got)
	}
	if got := truncate("abcdefghij", 3); got != "abcdefghij" {
		t.Errorf("max<4 should be a no-op, got %q", got)
	}
	if got := truncate("abcd", 4); got != "abcd" {
		t.Errorf("exact fit should be unchanged, got %q", got)
	}
	long := "internal/very/deep/path/to/somefile.go"
	got := truncate(long, 12)
	if rc := len([]rune(got)); rc != 12 {
		t.Errorf("truncated rune count = %d, want 12 (%q)", rc, got)
	}
	if !strings.HasPrefix(got, "…") {
		t.Errorf("truncated string should start with ellipsis, got %q", got)
	}
	if !strings.HasSuffix(long, strings.TrimPrefix(got, "…")) {
		t.Errorf("truncated string should keep the path tail, got %q", got)
	}
	// Multi-byte runes: byte-slicing would cut a rune in half and corrupt
	// the output; rune-slicing must not.
	multibyte := "проект/very/deep/文件夹/名前.go"
	got2 := truncate(multibyte, 10)
	if rc := len([]rune(got2)); rc != 10 {
		t.Errorf("multi-byte truncated rune count = %d, want 10 (%q)", rc, got2)
	}
	if !strings.HasSuffix(multibyte, strings.TrimPrefix(got2, "…")) {
		t.Errorf("multi-byte truncated string should keep the path tail intact, got %q", got2)
	}
}

// TestRootRepoShowsGit pins that a root whose git status comes back
// with a branch renders like a repo row (arrow, branch, its own dirty
// count) while keeping its bold name — not the plain umbrella Σ style.
func TestRootRepoShowsGit(t *testing.T) {
	m := NewModel(&fakeBackend{}, oneRepoOpts(&fakeBackend{}))
	var mod tea.Model = m
	mod, _ = mod.Update(gitMsg{"/root": {branch: "main", dirty: 2}})
	v := mod.View()
	if !strings.Contains(v, "root") {
		t.Errorf("missing root name in view:\n%s", v)
	}
	if !strings.Contains(v, "⎇main") {
		t.Errorf("missing root branch in view:\n%s", v)
	}
	if !strings.Contains(v, "[2]") {
		t.Errorf("missing root dirty count in view:\n%s", v)
	}
	if !strings.Contains(v, "▸") {
		t.Errorf("missing expand arrow on dirty root, view:\n%s", v)
	}
}

// TestRootRepoExpandsFiles pins that expanding a root-that's-a-repo
// inserts its file rows directly after the root row and BEFORE any
// child repo rows (proven here with a one-child-repo fixture), and
// that h collapses them again.
func TestRootRepoExpandsFiles(t *testing.T) {
	statusFn := func(string) (*gitx.Snapshot, error) { return &gitx.Snapshot{Branch: "main"}, nil }
	takeFn := func(dir string) (*gitx.Snapshot, error) {
		if dir != "/root" {
			return &gitx.Snapshot{Branch: "main"}, nil
		}
		return &gitx.Snapshot{
			Branch: "main",
			Staged: []gitx.FileChange{{Path: "a.go", Status: 'M', Added: 42, Deleted: 7}},
		}, nil
	}
	o := oneRepoOpts(nil)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(&fakeBackend{}, o)
	var mod tea.Model = m
	// Prime the root's git data (branch present) so it's known to be a
	// repo -- otherwise l on it is a no-op (TestUmbrellaRootUnchanged).
	mod, _ = mod.Update(gitMsg{"/root": {branch: "main", dirty: 1}})

	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // cursor is already on root (index 0); expand
	if cmd == nil {
		t.Fatal("expanding a root repo should return an immediate git refresh cmd")
	}
	mod, _ = mod.Update(cmd())

	sm := mod.(Model)
	rows := sm.rows()
	if len(rows) != 3 { // root, root's staged file, repo1
		t.Fatalf("rows after root expand = %+v", rows)
	}
	if rows[1].kind != rowFile || rows[1].name != "a.go" {
		t.Errorf("expected root's file row directly after the root row, got %+v", rows[1])
	}
	if rows[2].kind != rowRepo || rows[2].name != "repo1" {
		t.Errorf("expected child repo row after the root's file rows, got %+v", rows[2])
	}
	if v := sm.View(); !strings.Contains(v, "+42") {
		t.Errorf("missing root staged counts in view:\n%s", v)
	}

	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyLeft}) // collapse root
	sm = mod.(Model)
	if got := len(sm.rows()); got != 2 { // root, repo1
		t.Errorf("collapsed rows = %d, want 2: %+v", got, sm.rows())
	}
	if v := sm.View(); strings.Contains(v, "+42") {
		t.Errorf("collapsed root view should not show file rows:\n%s", v)
	}
}

// TestUmbrellaRootUnchanged pins that a root with no git data of its own
// (the common umbrella-folder case) keeps the pre-fix rendering exactly:
// bold name + Σ of child dirty counts, no ⎇/▸ ever, and l is a no-op.
func TestUmbrellaRootUnchanged(t *testing.T) {
	o := Options{
		WS: "ws", Root: "/root", RootName: "root",
		Repos: []gitx.Discovered{
			{Name: "repo0", Path: "/r0", Kind: gitx.DiscoveredRepo},
			{Name: "repo1", Path: "/r1", Kind: gitx.DiscoveredRepo},
		},
	}
	m := NewModel(&fakeBackend{}, o)
	var mod tea.Model = m
	mod, _ = mod.Update(gitMsg{
		"/r0": {branch: "main", dirty: 2},
		"/r1": {branch: "feature", dirty: 3},
	})

	sm := mod.(Model)
	rootLine := strings.Split(sm.View(), "\n")[1]
	if !strings.Contains(rootLine, "[5]") {
		t.Errorf("umbrella root row should show sum of child dirty, got %q", rootLine)
	}
	if strings.Contains(rootLine, "⎇") || strings.Contains(rootLine, "▸") || strings.Contains(rootLine, "▾") {
		t.Errorf("umbrella root row should never show branch/arrow, got %q", rootLine)
	}

	before := len(sm.rows())
	var cmd tea.Cmd
	mod, cmd = sm.Update(tea.KeyMsg{Type: tea.KeyRight}) // cursor is on root; umbrella root is a no-op
	if cmd != nil {
		t.Errorf("l on an umbrella root should not return a refresh cmd")
	}
	sm = mod.(Model)
	if got := len(sm.rows()); got != before {
		t.Errorf("l on umbrella root should be a no-op, rows before=%d after=%d", before, got)
	}
}

// TestRootFileRowParentWalk pins that h on one of the root's own file
// rows walks back to the root (not a child repo) and collapses it.
func TestRootFileRowParentWalk(t *testing.T) {
	statusFn := func(string) (*gitx.Snapshot, error) { return &gitx.Snapshot{Branch: "main"}, nil }
	takeFn := func(string) (*gitx.Snapshot, error) {
		return &gitx.Snapshot{Branch: "main", Untracked: []string{"root-file.txt"}}, nil
	}
	o := oneRepoOpts(nil)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(&fakeBackend{}, o)
	var mod tea.Model = m
	mod, _ = mod.Update(gitMsg{"/root": {branch: "main", dirty: 1}})

	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // expand root
	mod, _ = mod.Update(cmd())
	mod, _ = mod.Update(key("j")) // cursor -> root's file row

	sm := mod.(Model)
	rows := sm.rows()
	if sm.Cursor >= len(rows) || rows[sm.Cursor].kind != rowFile {
		t.Fatalf("setup: expected cursor on root's file row, rows=%+v cursor=%d", rows, sm.Cursor)
	}

	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyLeft})
	sm = mod.(Model)
	if sm.expanded["/root"] {
		t.Errorf("h on the root's own file row should collapse the root")
	}
	if sm.Cursor != 0 || sm.rows()[sm.Cursor].kind != rowRoot {
		t.Errorf("cursor should land back on the root row, cursor=%d rows=%+v", sm.Cursor, sm.rows())
	}
}

// TestRootWorktreeKidsListed pins the spec addendum: when the root is a
// repo with linked worktrees, they're listed as child rows right after
// the root row, named by bare basename (so their session is
// "<ws>/<basename>", with no root-name prefix), and selectable like any
// repo row.
func TestRootWorktreeKidsListed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wt := func(dir string) ([]gitx.Worktree, error) {
		if dir != "/root" {
			return nil, nil
		}
		return []gitx.Worktree{{Path: "/root-wt-a", Branch: "feature"}}, nil
	}
	b := &fakeBackend{}
	status := func(path string) (*gitx.Snapshot, error) {
		branch := "main"
		if path == "/root-wt-a" {
			branch = "feature"
		}
		return &gitx.Snapshot{Branch: branch}, nil
	}
	o := Options{WS: "ws", Root: "/root", RootName: "root", Cmd: "zsh", Worktrees: wt, Status: status}
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(gitMsg{"/root": {branch: "main", dirty: 0}}) // root confirmed a repo

	sm := mod.(Model)
	rows := sm.rows()
	if len(rows) != 2 { // root, root-wt-a
		t.Fatalf("rows = %+v", rows)
	}
	want := config.SessionName("ws", "root-wt-a")
	if rows[1].kind != rowRepo || rows[1].name != "root-wt-a" || rows[1].path != "/root-wt-a" || rows[1].session != want {
		t.Errorf("root worktree kid row = %+v", rows[1])
	}

	mod, _ = mod.Update(sessionsMsg{sessions: []tmux.SessionInfo{{Name: want}}})
	mod, _ = mod.Update(key("j")) // cursor -> kid row
	mod.Update(key("enter"))
	if len(b.switched) != 1 || b.switched[0] != want {
		t.Fatalf("switched = %v", b.switched)
	}
	sel, ok, err := state.Read("ws")
	if err != nil || !ok || sel.Session != want || sel.Path != "/root-wt-a" {
		t.Fatalf("state = %+v ok=%v err=%v", sel, ok, err)
	}
}

// TestRootWorktreeKidExpansionShowsFiles pins that expanding a root
// worktree kid (pressing 'l') shows its changed files when the git status
// is fetched, and collapsing ('h') hides them.
func TestRootWorktreeKidExpansionShowsFiles(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wt := func(dir string) ([]gitx.Worktree, error) {
		if dir != "/root" {
			return nil, nil
		}
		return []gitx.Worktree{{Path: "/root-wt-a", Branch: "feature"}}, nil
	}
	b := &fakeBackend{}
	o := Options{WS: "ws", Root: "/root", RootName: "root", Cmd: "zsh", Worktrees: wt}
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(gitMsg{"/root": {branch: "main", dirty: 0}}) // root confirmed a repo

	sm := mod.(Model)
	rows := sm.rows()
	if len(rows) != 2 { // root, root-wt-a
		t.Fatalf("rows before expand = %+v", rows)
	}

	// Navigate to kid row
	mod, _ = mod.Update(key("j")) // cursor -> kid row
	sm = mod.(Model)
	if sm.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1 (kid row)", sm.Cursor)
	}

	// Expand the kid
	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight})
	if cmd == nil {
		t.Fatal("expanding should return an immediate git refresh cmd")
	}
	mod, _ = mod.Update(cmd())

	// Deliver git status for the kid with a staged file
	mod, _ = mod.Update(gitMsg{
		"/root": {branch: "main", dirty: 0},
		"/root-wt-a": {
			branch: "feature",
			dirty:  1,
			snap: &gitx.Snapshot{
				Branch: "feature",
				Staged: []gitx.FileChange{{Path: "file.go", Status: 'M', Added: 42, Deleted: 0}},
			},
		},
	})

	sm = mod.(Model)
	rows = sm.rows()
	if len(rows) != 3 { // root, root-wt-a, file
		t.Fatalf("rows after expand with files = %+v", rows)
	}
	if rows[1].kind != rowRepo || rows[1].name != "root-wt-a" {
		t.Errorf("row 1 (kid) = %+v", rows[1])
	}
	if rows[2].kind != rowFile || rows[2].name != "file.go" || rows[2].added != 42 {
		t.Errorf("row 2 (file) = %+v", rows[2])
	}

	// Verify file appears in View
	if v := sm.View(); !strings.Contains(v, "+42") {
		t.Errorf("missing staged counts in view:\n%s", v)
	}

	// Collapse the kid
	sm.Cursor = 1
	mod = sm
	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyLeft})
	sm = mod.(Model)
	rows = sm.rows()
	if len(rows) != 2 {
		t.Errorf("collapsed rows = %d, want 2: %+v", len(rows), rows)
	}
	if v := sm.View(); strings.Contains(v, "+42") {
		t.Errorf("collapsed view should not show file rows:\n%s", v)
	}
}

// TestUmbrellaRootNoWorktreeKids pins the addendum's resolution: the
// worktree-discovery call for the root happens unconditionally at
// NewModel build time (mirroring buildRepos, and matching how prod's
// discoverWorktrees naturally returns nil for a non-repo dir), but the
// kids are never rendered as rows until the root's async git status
// confirms it's actually a repo.
func TestUmbrellaRootNoWorktreeKids(t *testing.T) {
	calls := 0
	wt := func(dir string) ([]gitx.Worktree, error) {
		calls++
		return []gitx.Worktree{{Path: "/root-wt-a", Branch: "feature"}}, nil
	}
	o := Options{WS: "ws", Root: "/root", RootName: "root", Worktrees: wt}
	m := NewModel(&fakeBackend{}, o)
	if calls != 1 {
		t.Fatalf("worktree discovery calls for root = %d, want 1 (unconditional)", calls)
	}
	rows := m.rows() // no gitMsg for root -> umbrella
	if len(rows) != 1 {
		t.Fatalf("umbrella root should render no worktree kid rows, got %+v", rows)
	}
}

func TestRootWorktreeDiscoveryErrorIsVisible(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{
		WS: "ws", Root: "/root", RootName: "root",
		Worktrees: func(string) ([]gitx.Worktree, error) {
			return nil, errors.New("worktree metadata unreadable")
		},
	})
	if !strings.Contains(m.ErrText, "worktree metadata unreadable") {
		t.Fatalf("ErrText = %q, want root worktree discovery failure", m.ErrText)
	}
}

// TestShutdownConfirmFlow pins the Q shutdown-confirm flow: Q shows a
// confirm prompt without touching anything, y calls
// backend.ShutdownWorkspace, and any other key cancels without calling it.
func TestShutdownConfirmFlow(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, oneRepoOpts(b))
	var mod tea.Model = m
	mod, _ = mod.Update(key("Q"))
	if b.shutdownCalls != 0 {
		t.Fatal("shutdown called before confirm")
	}
	if v := mod.View(); !strings.Contains(v, "shutdown workspace? y/n") {
		t.Errorf("expected shutdown confirm prompt, view:\n%s", v)
	}
	mod.Update(key("y"))
	if b.shutdownCalls != 1 {
		t.Errorf("shutdownCalls = %d, want 1", b.shutdownCalls)
	}
}

// TestShutdownConfirmCancel pins that any key other than y cancels the
// shutdown confirm without calling ShutdownWorkspace.
func TestShutdownConfirmCancel(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, oneRepoOpts(b))
	var mod tea.Model = m
	mod, _ = mod.Update(key("Q"))
	mod, _ = mod.Update(key("n"))
	if b.shutdownCalls != 0 {
		t.Errorf("shutdownCalls = %d, want 0 (cancelled)", b.shutdownCalls)
	}
	sm := mod.(Model)
	if sm.ConfirmShutdown {
		t.Error("confirmShutdown should be cleared after any key")
	}
}

// TestShutdownErrorSurfaced pins that a ShutdownWorkspace failure surfaces
// via errText — the pane survives to show it, per the skip-chrome-kill-
// on-error design in launcher.ShutdownWorkspace.
func TestShutdownErrorSurfaced(t *testing.T) {
	b := &fakeBackend{shutdownErr: fmt.Errorf("kill vb/x: boom")}
	m := NewModel(b, oneRepoOpts(b))
	var mod tea.Model = m
	mod, _ = mod.Update(key("Q"))
	mod, _ = mod.Update(key("y"))
	if v := mod.View(); !strings.Contains(v, "boom") {
		t.Errorf("expected shutdown error in view:\n%s", v)
	}
}

// TestShutdownConfirmGuardsMouse pins that mouse clicks are ignored while
// the shutdown confirm prompt is up, mirroring the kill-confirm guard.
func TestShutdownConfirmGuardsMouse(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, oneRepoOpts(b))
	var mod tea.Model = m
	mod, _ = mod.Update(key("Q"))
	mod, _ = mod.Update(tea.MouseMsg{Y: 0, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if len(b.switched) != 0 {
		t.Errorf("mouse click should be ignored during shutdown confirm: switched=%v", b.switched)
	}
	sm := mod.(Model)
	if !sm.ConfirmShutdown {
		t.Error("confirmShutdown should still be active after an ignored mouse click")
	}
}

// TestFileRowsNeverSwitchMiddle pins that file rows are never selectable
// the way repo/root rows are: Enter never calls SwitchMiddle for one
// (ShowDiff is a separate path — see the Test*OpensDiff tests below).
func TestFileRowsNeverSwitchMiddle(t *testing.T) {
	b := &fakeBackend{}
	statusFn := func(string) (*gitx.Snapshot, error) { return &gitx.Snapshot{Branch: "main"}, nil }
	takeFn := func(string) (*gitx.Snapshot, error) {
		return &gitx.Snapshot{Branch: "main", RepoRoot: "/r1", Untracked: []string{"c.txt"}}, nil
	}
	o := oneRepoOpts(b)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(key("j")) // cursor -> repo1

	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // expand
	mod, _ = mod.Update(cmd())
	mod, _ = mod.Update(key("j")) // cursor -> file row

	sm := mod.(Model)
	rows := sm.rows()
	if sm.Cursor >= len(rows) || rows[sm.Cursor].kind != rowFile {
		t.Fatalf("expected cursor on file row, rows=%+v cursor=%d", rows, sm.Cursor)
	}
	mod.Update(key("enter"))
	if len(b.switched) != 0 {
		t.Errorf("file row Enter should never call SwitchMiddle directly: switched=%v", b.switched)
	}
}

// TestFileRowEnterOpensDiffUntracked pins Enter on an untracked file row:
// ShowDiff is called with untracked=true, staged=false.
func TestFileRowEnterOpensDiffUntracked(t *testing.T) {
	b := &fakeBackend{}
	statusFn := func(string) (*gitx.Snapshot, error) { return &gitx.Snapshot{Branch: "main"}, nil }
	takeFn := func(string) (*gitx.Snapshot, error) {
		return &gitx.Snapshot{Branch: "main", RepoRoot: "/r1", Untracked: []string{"c.txt"}}, nil
	}
	o := oneRepoOpts(b)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(key("j")) // -> repo1
	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // expand
	mod, _ = mod.Update(cmd())
	mod, _ = mod.Update(key("j")) // -> c.txt
	mod, _ = mod.Update(key("enter"))

	sm := mod.(Model)
	if sm.rows()[sm.Cursor].kind != rowFile {
		t.Fatalf("setup: expected cursor on file row, rows=%+v cursor=%d", sm.rows(), sm.Cursor)
	}
	want := diffCall{repoRoot: "/r1", relPath: "c.txt", staged: false, untracked: true}
	if len(b.diffCalls) != 1 || b.diffCalls[0] != want {
		t.Errorf("diffCalls = %+v, want [%+v]", b.diffCalls, want)
	}
}

// TestFileRowEnterOpensDiffStaged pins Enter on a staged file row:
// staged=true, untracked=false.
func TestFileRowEnterOpensDiffStaged(t *testing.T) {
	b := &fakeBackend{}
	statusFn := func(string) (*gitx.Snapshot, error) { return &gitx.Snapshot{Branch: "main"}, nil }
	takeFn := func(string) (*gitx.Snapshot, error) {
		return &gitx.Snapshot{
			Branch:   "main",
			RepoRoot: "/r1",
			Staged:   []gitx.FileChange{{Path: "a.go", Status: 'M'}},
		}, nil
	}
	o := oneRepoOpts(b)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(key("j")) // -> repo1
	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // expand
	mod, _ = mod.Update(cmd())
	mod, _ = mod.Update(key("j")) // -> a.go (staged)
	mod.Update(key("enter"))

	want := diffCall{repoRoot: "/r1", relPath: "a.go", staged: true, untracked: false}
	if len(b.diffCalls) != 1 || b.diffCalls[0] != want {
		t.Errorf("diffCalls = %+v, want [%+v]", b.diffCalls, want)
	}
}

// TestFileRowEnterOpensDiffUnstaged pins Enter on an unstaged (not staged,
// not untracked) file row: both flags false, the plain working-tree diff.
func TestFileRowEnterOpensDiffUnstaged(t *testing.T) {
	b := &fakeBackend{}
	statusFn := func(string) (*gitx.Snapshot, error) { return &gitx.Snapshot{Branch: "main"}, nil }
	takeFn := func(string) (*gitx.Snapshot, error) {
		return &gitx.Snapshot{
			Branch:   "main",
			RepoRoot: "/r1",
			Unstaged: []gitx.FileChange{{Path: "b.go", Status: 'M'}},
		}, nil
	}
	o := oneRepoOpts(b)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(key("j")) // -> repo1
	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // expand
	mod, _ = mod.Update(cmd())
	mod, _ = mod.Update(key("j")) // -> b.go (unstaged)
	mod.Update(key("enter"))

	want := diffCall{repoRoot: "/r1", relPath: "b.go", staged: false, untracked: false}
	if len(b.diffCalls) != 1 || b.diffCalls[0] != want {
		t.Errorf("diffCalls = %+v, want [%+v]", b.diffCalls, want)
	}
}

// TestMouseClickThenEnterOpensDiff covers the click-then-Enter flow for
// file rows specifically: a click only moves the cursor (commit 2's
// behavior, unchanged here), and it's Enter that triggers the diff.
func TestMouseClickThenEnterOpensDiff(t *testing.T) {
	b := &fakeBackend{}
	statusFn := func(string) (*gitx.Snapshot, error) { return &gitx.Snapshot{Branch: "main"}, nil }
	takeFn := func(string) (*gitx.Snapshot, error) {
		return &gitx.Snapshot{Branch: "main", RepoRoot: "/r1", Untracked: []string{"c.txt"}}, nil
	}
	o := oneRepoOpts(b)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(key("j")) // -> repo1
	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // expand
	mod, _ = mod.Update(cmd())
	// rows: root(0), repo1(1), c.txt(2)
	mod, _ = mod.Update(tea.MouseMsg{Y: 3, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})

	sm := mod.(Model)
	if sm.Cursor != 2 {
		t.Fatalf("click should move cursor onto the file row, cursor=%d", sm.Cursor)
	}
	if len(b.diffCalls) != 0 {
		t.Fatalf("click alone should not open a diff: %v", b.diffCalls)
	}

	mod.Update(key("enter"))
	want := diffCall{repoRoot: "/r1", relPath: "c.txt", staged: false, untracked: true}
	if len(b.diffCalls) != 1 || b.diffCalls[0] != want {
		t.Errorf("diffCalls after enter = %+v, want [%+v]", b.diffCalls, want)
	}
}

// TestShowDiffNoOpWhenOwnerGitMissing guards the case a file row's owner
// has no git-status entry yet at all (shouldn't happen in practice — the
// row can't render without one — but the guard keeps this safe).
func TestShowDiffNoOpWhenOwnerGitMissing(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, oneRepoOpts(b))
	rows := []row{
		{kind: rowRoot, name: "root", path: "/root", session: "ws"},
		{kind: rowRepo, name: "repo1", path: "/r1", session: "ws/repo1"},
		{kind: rowFile, name: "a.go", status: 'M'},
	}
	m.activate(rows, 2)
	if len(b.diffCalls) != 0 {
		t.Errorf("ShowDiff should not fire without the owner's git status: %v", b.diffCalls)
	}
}

// TestShowDiffNoOpWhenSnapNotTaken guards the case the owner's git status
// is known (branch etc.) but its full snapshot (snap) hasn't been taken
// yet — e.g. the row was somehow reached before expansion completed.
func TestShowDiffNoOpWhenSnapNotTaken(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, oneRepoOpts(b))
	var mod tea.Model = m
	mod, _ = mod.Update(gitMsg{"/r1": {branch: "main"}}) // no snap yet
	sm := mod.(Model)
	rows := []row{
		{kind: rowRoot, name: "root", path: "/root", session: "ws"},
		{kind: rowRepo, name: "repo1", path: "/r1", session: "ws/repo1"},
		{kind: rowFile, name: "a.go", status: 'M'},
	}
	sm.activate(rows, 2)
	if len(b.diffCalls) != 0 {
		t.Errorf("ShowDiff should not fire without a taken snapshot: %v", b.diffCalls)
	}
}

// TestDualGroupSameFile pins that when the SAME path appears in both
// Staged and Unstaged with different flags, TWO file rows are rendered
// with opposite staged flags (staged first), and Enter on each calls
// ShowDiff with the correct flag for that group.
func TestDualGroupSameFile(t *testing.T) {
	b := &fakeBackend{}
	statusFn := func(string) (*gitx.Snapshot, error) { return &gitx.Snapshot{Branch: "main"}, nil }
	takeFn := func(string) (*gitx.Snapshot, error) {
		return &gitx.Snapshot{
			Branch:   "main",
			RepoRoot: "/r1",
			// both.go appears in both Staged (with Added=1) and Unstaged (with Added=2)
			Staged:   []gitx.FileChange{{Path: "both.go", Status: 'M', Added: 1}},
			Unstaged: []gitx.FileChange{{Path: "both.go", Status: 'M', Added: 2}},
		}, nil
	}
	o := oneRepoOpts(b)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(key("j")) // -> repo1

	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // expand
	mod, _ = mod.Update(cmd())

	sm := mod.(Model)
	rows := sm.rows()
	// Expected: root(0), repo1(1), both.go staged(2), both.go unstaged(3)
	if len(rows) != 4 {
		t.Fatalf("rows = %+v, want 4 rows (root, repo, staged both.go, unstaged both.go)", rows)
	}
	if rows[2].name != "both.go" || !rows[2].staged {
		t.Errorf("row 2 (staged) = %+v, want both.go with staged=true", rows[2])
	}
	if rows[3].name != "both.go" || rows[3].staged {
		t.Errorf("row 3 (unstaged) = %+v, want both.go with staged=false", rows[3])
	}

	// Enter on row 2 (staged) should call ShowDiff with staged=true
	mod, _ = mod.Update(key("j")) // cursor: repo1 -> staged both.go
	mod.Update(key("enter"))

	if len(b.diffCalls) != 1 {
		t.Fatalf("diffCalls = %+v, want 1 call", b.diffCalls)
	}
	want := diffCall{repoRoot: "/r1", relPath: "both.go", staged: true, untracked: false}
	if b.diffCalls[0] != want {
		t.Errorf("diffCalls[0] = %+v, want %+v", b.diffCalls[0], want)
	}

	// Clear for the second test
	b.diffCalls = nil

	// Move cursor to row 3 (unstaged both.go) and Enter
	mod, _ = mod.Update(key("j"))
	sm = mod.(Model)
	if sm.Cursor != 3 {
		t.Fatalf("cursor = %d, want 3", sm.Cursor)
	}
	mod.Update(key("enter"))

	if len(b.diffCalls) != 1 {
		t.Fatalf("diffCalls after second enter = %+v, want 1 call", b.diffCalls)
	}
	want = diffCall{repoRoot: "/r1", relPath: "both.go", staged: false, untracked: false}
	if b.diffCalls[0] != want {
		t.Errorf("diffCalls[0] = %+v, want %+v", b.diffCalls[0], want)
	}
}

// TestLongPathPassthrough pins that a file row whose path exceeds the
// render-width budget gets truncated visually but ShowDiff receives the
// FULL untruncated path. Set model width small so truncation is visible,
// navigate to the long-path row, Enter, and verify the backend gets the
// full path.
func TestLongPathPassthrough(t *testing.T) {
	b := &fakeBackend{}
	statusFn := func(string) (*gitx.Snapshot, error) { return &gitx.Snapshot{Branch: "main"}, nil }
	longPath := "internal/very/deep/path/to/some/file/that/is/quite/long/longfile.go" // ~60 chars
	takeFn := func(string) (*gitx.Snapshot, error) {
		return &gitx.Snapshot{
			Branch:    "main",
			RepoRoot:  "/r1",
			Untracked: []string{longPath},
		}, nil
	}
	o := oneRepoOpts(b)
	o.Status, o.Take = statusFn, takeFn
	m := NewModel(b, o)
	var mod tea.Model = m
	mod, _ = mod.Update(key("j")) // -> repo1

	// Set small width so truncation happens
	mod, _ = mod.Update(tea.WindowSizeMsg{Width: 40, Height: 20})

	var cmd tea.Cmd
	mod, cmd = mod.Update(tea.KeyMsg{Type: tea.KeyRight}) // expand
	mod, _ = mod.Update(cmd())

	sm := mod.(Model)
	rows := sm.rows()
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want 3 (root, repo, longfile)", rows)
	}
	if rows[2].name != longPath {
		t.Errorf("row 2 name = %q, want %q (untruncated internally)", rows[2].name, longPath)
	}

	// Navigate to the long file row and Enter
	mod, _ = mod.Update(key("j")) // cursor: repo1 -> long file
	mod.Update(key("enter"))

	if len(b.diffCalls) != 1 {
		t.Fatalf("diffCalls = %+v, want 1", b.diffCalls)
	}
	// Verify the backend received the full path
	if b.diffCalls[0].relPath != longPath {
		t.Errorf("relPath = %q, want %q (full, untruncated)", b.diffCalls[0].relPath, longPath)
	}
}

// `git worktree list` always reports a repo's MAIN worktree alongside any
// linked ones. Emitting a child row for it duplicated every ordinary
// checkout: one repo became two rows over one directory, and so two tmux
// sessions.
func TestBuildReposSkipsTheRepoItself(t *testing.T) {
	repos := []gitx.Discovered{{Name: "solo", Path: "/ws/solo", Kind: gitx.DiscoveredRepo}}
	wt := func(dir string) ([]gitx.Worktree, error) {
		return []gitx.Worktree{{Path: "/ws/solo", Branch: "main"}}, nil
	}
	got, errText := buildRepos(repos, wt)
	if errText != "" {
		t.Fatalf("unexpected errText %q", errText)
	}
	if len(got) != 1 || got[0].Name != "solo" || got[0].Path != "/ws/solo" {
		t.Errorf("a lone repo should stay one row, got %+v", got)
	}
}

// A linked worktree living beside its repo is itself discovered as a repo,
// and `git worktree list` returns BOTH from EITHER — so expanding each
// produced the full cross product: 2 directories became 6 rows and 6
// sessions, with 3 different session names per directory.
func TestBuildReposDoesNotCrossProductSiblingWorktrees(t *testing.T) {
	repos := []gitx.Discovered{
		{Name: "foo", Path: "/ws/foo", Kind: gitx.DiscoveredRepo},
		{Name: "foo-feat", Path: "/ws/foo-feat", Kind: gitx.DiscoveredRepo},
	}
	both := []gitx.Worktree{{Path: "/ws/foo", Branch: "main"}, {Path: "/ws/foo-feat", Branch: "feat"}}
	wt := func(dir string) ([]gitx.Worktree, error) { return both, nil }
	got, _ := buildRepos(repos, wt)
	if len(got) != 2 {
		t.Fatalf("2 directories should yield 2 rows, got %d: %+v", len(got), got)
	}
	seen := map[string]int{}
	for _, d := range got {
		seen[d.Path]++
	}
	for path, n := range seen {
		if n != 1 {
			t.Errorf("%s appears in %d rows; each directory must own exactly one session", path, n)
		}
	}
}

// A worktree the walker did NOT discover on its own (nested, or outside
// the workspace root) is still surfaced as a child of its repo.
func TestBuildReposKeepsUndiscoveredWorktrees(t *testing.T) {
	repos := []gitx.Discovered{{Name: "foo", Path: "/ws/foo", Kind: gitx.DiscoveredRepo}}
	wt := func(dir string) ([]gitx.Worktree, error) {
		return []gitx.Worktree{
			{Path: "/ws/foo", Branch: "main"},
			{Path: "/elsewhere/foo-hotfix", Branch: "hotfix"},
		}, nil
	}
	got, _ := buildRepos(repos, wt)
	if len(got) != 2 {
		t.Fatalf("want repo + its undiscovered worktree, got %+v", got)
	}
	if got[1].Name != "foo/foo-hotfix" || got[1].Path != "/elsewhere/foo-hotfix" {
		t.Errorf("worktree child wrong: %+v", got[1])
	}
	if got[1].Kind != gitx.DiscoveredWorktree {
		t.Errorf("worktree child should be Kind=DiscoveredWorktree, got %v", got[1].Kind)
	}
}

// The root's own main worktree is reported by `git worktree list` and must
// not become a kid row duplicating the root itself.
func TestRootWorktreeKidsSkipsTheRootAndKnownPaths(t *testing.T) {
	wt := func(dir string) ([]gitx.Worktree, error) {
		return []gitx.Worktree{
			{Path: "/ws", Branch: "main"},          // the root itself
			{Path: "/ws/already", Branch: "known"}, // already a walker row
			{Path: "/ws/fresh", Branch: "fresh"},   // genuinely new
		}, nil
	}
	known := []gitx.Discovered{{Name: "already", Path: "/ws/already", Kind: gitx.DiscoveredRepo}}
	got, err := rootWorktreeKids("/ws", known, wt)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("only the undiscovered worktree should become a kid, got %+v", got)
	}
	if got[0].Path != "/ws/fresh" || got[0].Name != "fresh" {
		t.Errorf("kid = %+v, want the fresh worktree", got[0])
	}
}

// The walker builds paths with filepath.Abs (symlinks unresolved) while
// `git worktree list` reports resolved ones. A workspace opened through a
// symlink therefore produced two spellings of one directory, no dedupe key
// matched, and the duplicate rows and sessions came back.
func TestBuildReposDedupesAcrossSymlinkedPaths(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// The walker found the repos through the symlink...
	repos := []gitx.Discovered{
		{Name: "foo", Path: filepath.Join(link, "foo"), Kind: gitx.DiscoveredRepo},
		{Name: "foo-feat", Path: filepath.Join(link, "foo-feat"), Kind: gitx.DiscoveredRepo},
	}
	for _, d := range repos {
		if err := os.MkdirAll(d.Path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// ...while git reports the resolved ones.
	wt := func(string) ([]gitx.Worktree, error) {
		return []gitx.Worktree{
			{Path: filepath.Join(real, "foo"), Branch: "main"},
			{Path: filepath.Join(real, "foo-feat"), Branch: "feat"},
		}, nil
	}
	got, _ := buildRepos(repos, wt)
	if len(got) != 2 {
		names := []string{}
		for _, d := range got {
			names = append(names, d.Name)
		}
		t.Errorf("2 directories should yield 2 rows through a symlink, got %d: %v", len(got), names)
	}
}

// The tree shows the same per-session status the monitor does, so a failed
// poll must not blank it either. See the terms pane's counterpart for why
// an empty list is the wrong reading of an error.
func TestTransientSessionsFailureKeepsTreeSessions(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb", Root: t.TempDir()})
	var mod tea.Model = m
	mod, _ = mod.Update(sessionsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb/api", Bell: true},
	}})
	before := mod.(Model)
	if len(before.sessions) != 1 {
		t.Fatalf("precondition: %d sessions, want 1", len(before.sessions))
	}

	b.sessionsErr = tmux.ErrNoServer
	mod, _ = mod.Update(before.fetchSessions()())
	after := mod.(Model)

	if len(after.sessions) != 1 {
		t.Errorf("sessions = %d, want 1 — one failed poll erased the tree's status", len(after.sessions))
	}
	if !after.sessions["vb/api"].Bell {
		t.Error("the bell on vb/api was cleared by a failed poll")
	}
}

// And, as in the monitor, holding those rows over must be visible. The
// tree's git column and session dots both go quietly wrong otherwise.
func TestFailedTreePollSaysTheRowsAreStale(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb", Root: t.TempDir()})
	var mod tea.Model = m
	mod, _ = mod.Update(mod.(Model).fetchSessions()())
	if got := mod.(Model).footer(); strings.Contains(got, "stale") {
		t.Fatalf("precondition: footer = %q on a healthy poll", got)
	}

	b.sessionsErr = errors.New("no server running")
	mod, _ = mod.Update(mod.(Model).fetchSessions()())
	if got := mod.(Model).footer(); !strings.Contains(got, "no server running") {
		t.Errorf("footer = %q after a failed poll; the tree is frozen and nothing says so", got)
	}

	b.sessionsErr = nil
	mod, _ = mod.Update(mod.(Model).fetchSessions()())
	if got := mod.(Model).footer(); strings.Contains(got, "no server running") {
		t.Errorf("footer = %q once polling recovered; the warning outlived the staleness", got)
	}
}

func TestEntryNamesMatchRenderedRepoAndWorktreeRows(t *testing.T) {
	repos := []gitx.Discovered{{
		Name: "api server",
		Path: "/repos/api",
		Kind: gitx.DiscoveredRepo,
	}}
	worktrees := func(path string) ([]gitx.Worktree, error) {
		switch path {
		case "/workspace":
			return []gitx.Worktree{{Path: "/workspace"}, {Path: "/worktrees/root fix"}}, nil
		case "/repos/api":
			return []gitx.Worktree{{Path: "/repos/api"}, {Path: "/worktrees/api fix"}}, nil
		default:
			return nil, nil
		}
	}
	got, errText := entryNames("/workspace", repos, worktrees)
	if errText != "" {
		t.Fatal(errText)
	}
	if joined := strings.Join(got, "|"); joined != "root fix|api server|api server/api fix" {
		t.Fatalf("entryNames = %v", got)
	}
}

// Two distinct directories can have the same derived entry name: most
// commonly two root-level linked worktrees that share a basename. Because
// the entry name is also the tmux session identity, rendering either row
// would let one directory silently reuse the other's terminal.
func TestEntryTopologyRejectsDuplicateDerivedNames(t *testing.T) {
	repos := []gitx.Discovered{{
		Name: "same",
		Path: "/workspace/same",
		Kind: gitx.DiscoveredRepo,
	}}
	worktrees := func(path string) ([]gitx.Worktree, error) {
		if path == "/workspace" {
			return []gitx.Worktree{
				{Path: "/workspace"},
				{Path: "/elsewhere/same"},
			}, nil
		}
		return nil, nil
	}

	expanded, rootKids, errText := entryTopology("/workspace", "workspace", repos, worktrees)
	if len(expanded) != 0 || len(rootKids) != 0 {
		t.Fatalf("ambiguous entries were rendered: repos=%+v rootKids=%+v", expanded, rootKids)
	}
	for _, want := range []string{"duplicate entry name \"same\"", "/workspace/same", "/elsewhere/same"} {
		if !strings.Contains(errText, want) {
			t.Errorf("errText = %q, want %q", errText, want)
		}
	}

	names, namesErr := entryNames("/workspace", repos, worktrees)
	if len(names) != 0 || !strings.Contains(namesErr, "duplicate entry name") {
		t.Fatalf("migration topology admitted ambiguity: names=%v err=%q", names, namesErr)
	}
}
