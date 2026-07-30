package terms

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/sarcasticbird/wrap/internal/config"
	mirrorapi "github.com/sarcasticbird/wrap/internal/mirror"
	"github.com/sarcasticbird/wrap/internal/state"
	"github.com/sarcasticbird/wrap/internal/tmux"
)

type fakeMirror struct {
	events     chan mirrorapi.Event
	snapshot   mirrorapi.Snapshot
	mirrored   []mirrorapi.HostSession
	revoked    []mirrorapi.Identity
	rotations  int
	reconciles [][]mirrorapi.HostSession
}

func newFakeMirror() *fakeMirror {
	return &fakeMirror{events: make(chan mirrorapi.Event, 8)}
}

func (m *fakeMirror) Events() <-chan mirrorapi.Event { return m.events }
func (m *fakeMirror) Snapshot() mirrorapi.Snapshot   { return m.snapshot }
func (m *fakeMirror) Mirror(_ context.Context, session mirrorapi.HostSession) error {
	m.mirrored = append(m.mirrored, session)
	return nil
}
func (m *fakeMirror) Revoke(_ context.Context, identity mirrorapi.Identity) error {
	m.revoked = append(m.revoked, identity)
	return nil
}
func (m *fakeMirror) Rotate(context.Context) error {
	m.rotations++
	return nil
}
func (m *fakeMirror) Reconcile(_ context.Context, sessions []mirrorapi.HostSession) error {
	m.reconciles = append(m.reconciles, sessions)
	return nil
}

type fakeBackend struct {
	sessionsErr      error
	displayed        string
	displayErr       error
	alerts           []bool
	alertErr         error
	rings            int
	ringErr          error
	killSuccessor    string
	killID           string
	killGeneration   string
	switched, killed []string
	newTermCalls     []string
	newTermName      string
	newTermErr       error
	renameCalls      []string
	renameName       string
	renameErr        error
	ensureCalls      []string
	ensureErr        error
	detached         bool
	sessions         []tmux.SessionInfo
	shutdownCalls    int
	shutdownErr      error
	pathByIdentity   map[string]string
	pathErrIdentity  map[string]error
	pathCalls        []string
	scratchKillCalls int
}

func (f *fakeBackend) ShutdownWorkspace() error {
	f.shutdownCalls++
	return f.shutdownErr
}

func (f *fakeBackend) Sessions() ([]tmux.SessionInfo, error) {
	if f.sessionsErr != nil {
		return nil, f.sessionsErr
	}
	return f.sessions, nil
}

func (f *fakeBackend) DisplayedSession() (string, error) {
	return f.displayed, f.displayErr
}

func (f *fakeBackend) SessionCurrentPath(id, generation string) (string, error) {
	key := generation + "|" + id
	f.pathCalls = append(f.pathCalls, key)
	if err := f.pathErrIdentity[key]; err != nil {
		return "", err
	}
	return f.pathByIdentity[key], nil
}

// ShowInMiddle (unlike the tree's SwitchMiddle) never moves pane focus —
// this fake has no select-pane concept at all, so that guarantee is
// structural: terms simply has no way to invoke it.
func (f *fakeBackend) ShowInMiddle(target string) error {
	f.switched = append(f.switched, target)
	return nil
}

func (f *fakeBackend) SetWorkspaceAlert(alert bool) error {
	f.alerts = append(f.alerts, alert)
	return f.alertErr
}

func (f *fakeBackend) RingWorkspaceAlert() error {
	f.rings++
	return f.ringErr
}

func (f *fakeBackend) KillEntrySession(name, targetID, targetGeneration, successor string) error {
	f.killSuccessor = successor
	f.killID = targetID
	f.killGeneration = targetGeneration
	f.killed = append(f.killed, name)
	return nil
}

func (f *fakeBackend) KillScratchSession(name, targetID, targetGeneration, successor string) error {
	f.scratchKillCalls++
	return f.KillEntrySession(name, targetID, targetGeneration, successor)
}

func (f *fakeBackend) EnsureEntrySession(name, dir, cmd string) error {
	f.ensureCalls = append(f.ensureCalls, name+"|"+dir+"|"+cmd)
	return f.ensureErr
}

func (f *fakeBackend) DetachUI() error {
	f.detached = true
	return nil
}

func (f *fakeBackend) NewTerm(dir, cmd string) (string, error) {
	f.newTermCalls = append(f.newTermCalls, dir+"|"+cmd)
	if f.newTermErr != nil {
		return "", f.newTermErr
	}
	name := f.newTermName
	if name == "" {
		name = "vb·term·1"
	}
	return name, nil
}

func (f *fakeBackend) RenameTerm(oldName, targetID, targetGeneration, label string) (string, error) {
	f.renameCalls = append(f.renameCalls, oldName+"|"+targetID+"|"+targetGeneration+"|"+label)
	if f.renameErr != nil {
		return "", f.renameErr
	}
	name := f.renameName
	if name == "" {
		name = oldName
	}
	return name, nil
}

func key(s string) tea.KeyMsg {
	if s == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func TestMirrorEligibilityAndRowMarker(t *testing.T) {
	for _, test := range []struct {
		name string
		row  row
		want bool
	}{
		{"root", row{name: "vb", kind: ""}, true},
		{"entry", row{name: "vb/api", kind: tmux.SessionKindEntry}, true},
		{"scratch", row{name: "vb·term·1", kind: tmux.SessionKindScratch}, true},
		{"legacy", row{name: "vb/legacy"}, true},
		{"diff", row{name: "vb/diff", kind: tmux.SessionKindDiff}, false},
		{"other workspace", row{name: "other/api", kind: tmux.SessionKindEntry}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := mirrorEligible("vb", test.row); got != test.want {
				t.Fatalf("mirrorEligible = %v, want %v", got, test.want)
			}
		})
	}
	got := renderRow(row{name: "vb/api", mirrored: true, bell: true, activity: true}, 80)
	if !strings.Contains(got, "📡") || !strings.Contains(got, "🔔") {
		t.Fatalf("mirrored bell row = %q", got)
	}
}

func TestMirrorKeyStartsEligibleRowAndOverlayBlocksOrdinaryKeys(t *testing.T) {
	mirrors := newFakeMirror()
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: "/workspace", Mirrors: mirrors})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		ID: "$7", Generation: "generation-a", Name: "vb/api", Kind: tmux.SessionKindEntry,
	}}})
	mod, cmd := mod.Update(key("m"))
	got := mod.(Model)
	if !got.mirrorOpen || cmd == nil {
		t.Fatalf("mirror overlay/cmd = %v/%v", got.mirrorOpen, cmd)
	}
	if msg := cmd(); msg == nil {
		t.Fatal("mirror command returned no result message")
	}
	if len(mirrors.mirrored) != 1 || mirrors.mirrored[0].ID != "$7" {
		t.Fatalf("mirror calls = %+v", mirrors.mirrored)
	}
	before := got.Cursor
	mod, _ = got.Update(key("j"))
	if mod.(Model).Cursor != before {
		t.Fatal("navigation escaped the mirror overlay")
	}
}

func TestMirrorOperationMessagesIgnoreStaleCompletionsAndSnapshots(t *testing.T) {
	mirrors := newFakeMirror()
	canceled := false
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Mirrors: mirrors})
	m.mirrorOpen = true
	m.mirrorStarting = true
	m.mirrorOperationID = 2
	m.mirrorCancel = func() { canceled = true }

	staleSnapshot := mirrorapi.Snapshot{State: mirrorapi.StateStopped}
	mod, _ := m.Update(mirrorEventMsg{
		event: mirrorapi.Event{Snapshot: &staleSnapshot},
		ok:    true,
	})
	got := mod.(Model)
	if !got.mirrorStarting || canceled {
		t.Fatal("stale snapshot canceled the active mirror operation")
	}

	mod, _ = got.Update(mirrorOperationMsg{
		operation: "revoke",
		token:     1,
	})
	got = mod.(Model)
	if !got.mirrorStarting || !got.mirrorOpen || canceled {
		t.Fatal("stale operation completion mutated the active mirror operation")
	}

	mod, _ = got.Update(mirrorOperationMsg{
		operation: "start",
		token:     2,
	})
	got = mod.(Model)
	if got.mirrorStarting || !canceled {
		t.Fatal("current operation completion did not clear its cancellation state")
	}
}

func TestMirrorOverlayPreservesPairingURLWhenQRDoesNotFit(t *testing.T) {
	const pairingURL = "https://quiet-river.trycloudflare.com/#k=abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	m := Model{
		mirrorOpen:       true,
		mirrorTargetName: "vb/api",
		mirrorSnapshot: mirrorapi.Snapshot{
			State:      mirrorapi.StateReady,
			PairingURL: pairingURL,
			QR:         "QR-LINE-ONE\nQR-LINE-TWO\nQR-LINE-THREE\nQR-LINE-FOUR",
			Sessions:   []mirrorapi.Session{{Name: "vb/api"}},
		},
	}
	m.Width = 30
	m.Height = 8
	view := ansi.Strip(m.mirrorView("Terminals"))
	if !strings.Contains(strings.ReplaceAll(view, "\n", ""), pairingURL) {
		t.Fatalf("narrow mirror overlay omitted or truncated pairing URL:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if runewidth.StringWidth(line) > m.Width {
			t.Fatalf("narrow mirror overlay emitted %d-cell line:\n%s", runewidth.StringWidth(line), view)
		}
	}
	if strings.Contains(view, "QR-LINE") {
		t.Fatalf("narrow mirror overlay rendered a partial QR:\n%s", view)
	}
}

func TestMirrorOverlayScrollsThroughCompletePairingURL(t *testing.T) {
	const pairingURL = "https://quiet-river.trycloudflare.com/#k=abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	m := Model{
		mirrorOpen:       true,
		mirrorTargetName: "vb/api",
		mirrorSnapshot: mirrorapi.Snapshot{
			State:      mirrorapi.StateReady,
			PairingURL: pairingURL,
			Sessions:   []mirrorapi.Session{{Name: "vb/api"}},
		},
	}
	m.Width = 12
	m.Height = 4
	urlLines := mirrorWrapLine(pairingURL, m.Width)
	if m.mirrorMaxScroll() == 0 {
		t.Fatal("short overlay did not expose a scroll range")
	}
	for _, want := range urlLines {
		found := false
		for offset := 0; offset <= m.mirrorMaxScroll(); offset++ {
			m.mirrorScroll = offset
			if strings.Contains(ansi.Strip(m.mirrorView("Terminals")), want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("wrapped credential chunk %q is unreachable", want)
		}
	}
	m.mirrorScroll = 0
	mod, _ := m.Update(key("down"))
	if mod.(Model).mirrorScroll != 1 {
		t.Fatal("down did not scroll the mirror overlay")
	}
	mod, _ = mod.Update(key("up"))
	if mod.(Model).mirrorScroll != 0 {
		t.Fatal("up did not scroll the mirror overlay")
	}
}

func writeMalformedSelection(t *testing.T, ws string) {
	t.Helper()
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	dir := filepath.Join(stateHome, "wrap", ws)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPollSchedulesNextOnlyAfterCompletion(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	m := NewModel(&fakeBackend{}, Options{WS: "ws", Root: t.TempDir()})
	mod, cmd := m.Update(tickMsg{})
	if cmd == nil {
		t.Fatal("tick did not start a rows poll")
	}
	msg := cmd()
	if _, ok := msg.(rowsMsg); !ok {
		t.Fatalf("tick command returned %T, want one rowsMsg (no overlapping timer batch)", msg)
	}
	_, next := mod.Update(msg)
	if next == nil {
		t.Fatal("completed rows poll did not schedule the next tick")
	}
}

func TestManualRefreshDoesNotCreateSecondTimerChain(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "ws", Root: t.TempDir()})
	m.polling = true
	m.timerPending = true
	mod, next := m.Update(rowsMsg{})
	got := mod.(Model)
	if next != nil {
		t.Fatal("manual refresh scheduled a timer while the regular timer was still pending")
	}
	if !got.timerPending || got.polling {
		t.Fatalf("timer/poll state = pending:%v polling:%v", got.timerPending, got.polling)
	}
}

func TestRightExpandsSelectedIdentityAndLeftCollapsesIt(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: "/workspace"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{ID: "$7", Generation: "generation-a", Name: "vb/api"},
		{ID: "$8", Generation: "generation-a", Name: "vb/web"},
	}})
	mod, cmd := mod.Update(tea.KeyMsg{Type: tea.KeyRight})
	got := mod.(Model)
	identity := sessionKey{generation: "generation-a", id: "$7"}
	if !got.expanded[identity] {
		t.Fatalf("Right did not expand selected identity: %v", got.expanded)
	}
	if cmd == nil {
		t.Fatal("Right should request an immediate details refresh")
	}

	mod, _ = mod.Update(key("j"))
	if !mod.(Model).expanded[identity] {
		t.Fatal("navigating away collapsed an independently expanded session")
	}
	mod, _ = mod.Update(key("k"))
	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyLeft})
	if mod.(Model).expanded[identity] {
		t.Fatal("Left did not collapse selected identity")
	}
}

func TestExpansionPersistsAcrossRenameButDropsOnReplacement(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		ID: "$7", Generation: "generation-a", Name: "vb·term·1",
	}}})
	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyRight})
	key := sessionKey{generation: "generation-a", id: "$7"}

	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		ID: "$7", Generation: "generation-a", Name: "vb·term·logs",
	}}})
	if !mod.(Model).expanded[key] {
		t.Fatal("same session identity lost expansion after rename")
	}

	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		ID: "$8", Generation: "generation-a", Name: "vb·term·logs",
	}}})
	if len(mod.(Model).expanded) != 0 {
		t.Fatalf("replacement session inherited expansion: %v", mod.(Model).expanded)
	}

	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyRight})
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		ID: "$8", Generation: "generation-b", Name: "vb·term·logs",
	}}})
	if len(mod.(Model).expanded) != 0 {
		t.Fatalf("server generation replacement inherited expansion: %v", mod.(Model).expanded)
	}
}

func TestFetchRequestsPWDOnlyForExpandedSessions(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := &fakeBackend{
		sessions: []tmux.SessionInfo{
			{ID: "$7", Generation: "generation-a", Name: "vb/api"},
			{ID: "$8", Generation: "generation-a", Name: "vb/web"},
		},
		pathByIdentity: map[string]string{
			"generation-a|$7": "/workspace/api",
			"generation-a|$8": "/workspace/web",
		},
	}
	m := NewModel(b, Options{WS: "vb", Root: "/workspace"})
	m.expanded[sessionKey{generation: "generation-a", id: "$8"}] = true
	msg := m.fetch()().(rowsMsg)
	if msg.err != nil {
		t.Fatal(msg.err)
	}
	if len(b.pathCalls) != 1 || b.pathCalls[0] != "generation-a|$8" {
		t.Fatalf("path calls = %v, want only expanded session", b.pathCalls)
	}
}

func TestRenderPathUsesCompactDetailLayout(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: "/workspace"})
	tests := []struct {
		name string
		path pathState
		want string
	}{
		{name: "path", path: pathState{value: "/workspace/api", valid: true}, want: "    /workspace/api"},
		{name: "stale", path: pathState{value: "/workspace/api", valid: true, stale: true}, want: "    /workspace/api ?"},
		{name: "unavailable", path: pathState{}, want: "    ⚠ unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ansi.Strip(m.renderPath(tt.path)); got != tt.want {
				t.Fatalf("renderPath = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPWDResultAppliesOnlyToMatchingIdentity(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: "/workspace"})
	m.sessions["vb/api"] = tmux.SessionInfo{
		ID: "$8", Generation: "generation-b", Name: "vb/api",
	}
	key := sessionKey{generation: "generation-b", id: "$8"}
	m.expanded[key] = true
	mod, _ := m.Update(rowsMsg{
		sessions: []tmux.SessionInfo{{
			ID: "$8", Generation: "generation-b", Name: "vb/api",
		}},
		paths: map[sessionKey]pathResult{
			{generation: "generation-a", id: "$7"}: {path: "/wrong"},
			key:                                    {path: "/workspace/api"},
		},
	})
	view := mod.View()
	if !strings.Contains(view, "    /workspace/api") || strings.Contains(view, "/wrong") {
		t.Fatalf("identity-anchored PWD not applied correctly:\n%s", view)
	}
}

func TestPWDFirstFailureAndStaleRecovery(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	key := sessionKey{generation: "generation-a", id: "$7"}
	b := &fakeBackend{
		sessions: []tmux.SessionInfo{{
			ID: "$7", Generation: "generation-a", Name: "vb/api",
		}},
		pathByIdentity:  map[string]string{"generation-a|$7": "/workspace/api"},
		pathErrIdentity: map[string]error{"generation-a|$7": errors.New("pane unavailable")},
	}
	m := NewModel(b, Options{WS: "vb", Root: "/workspace"})
	m.expanded[key] = true

	mod, _ := m.Update(m.fetch()())
	if view := mod.View(); !strings.Contains(view, "    ⚠ unavailable") {
		t.Fatalf("first failure did not render unavailable detail:\n%s", view)
	}

	delete(b.pathErrIdentity, "generation-a|$7")
	mod, _ = mod.Update(mod.(Model).fetch()())
	if view := mod.View(); !strings.Contains(view, "    /workspace/api") ||
		strings.Contains(view, "    /workspace/api ?") {
		t.Fatalf("successful PWD did not clear failure:\n%s", view)
	}

	b.pathErrIdentity["generation-a|$7"] = errors.New("pane unavailable")
	mod, _ = mod.Update(mod.(Model).fetch()())
	if view := mod.View(); !strings.Contains(view, "    /workspace/api ?") {
		t.Fatalf("later failure did not retain and mark last path stale:\n%s", view)
	}

	delete(b.pathErrIdentity, "generation-a|$7")
	b.pathByIdentity["generation-a|$7"] = "/workspace/api/subdir"
	mod, _ = mod.Update(mod.(Model).fetch()())
	if view := mod.View(); !strings.Contains(view, "    /workspace/api/subdir") ||
		strings.Contains(view, "/workspace/api/subdir ?") {
		t.Fatalf("recovery did not replace and clear stale PWD:\n%s", view)
	}
}

func TestFormatPWDAbsoluteAndControlSafe(t *testing.T) {
	tests := []struct {
		name, path, want string
	}{
		{"root", "/workspace", "/workspace"},
		{"inside", "/workspace/main/frontend", "/workspace/main/frontend"},
		{"outside", "/tmp/project", "/tmp/project"},
		{"control characters", "/workspace/line\nwith\ttab\x1b", `/workspace/line\nwith\ttab\x1b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatPWD(tt.path); got != tt.want {
				t.Fatalf("formatPWD = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTruncateLeftUsesTerminalCellWidth(t *testing.T) {
	if got := truncateLeft("short", 5); got != "short" {
		t.Fatalf("exact fit = %q", got)
	}
	for _, tt := range []struct {
		name, value string
		width       int
		want        string
	}{
		{"slightly wider", "abcdefghijk", 10, "…cdefghijk"},
		{"substantially wider", "abcdefghijklmnopqrstuvwxyz", 10, "…rstuvwxyz"},
		{"ellipsis only", "abcdef", 1, "…"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateLeft(tt.value, tt.width); got != tt.want {
				t.Fatalf("truncateLeft(%q, %d) = %q, want %q", tt.value, tt.width, got, tt.want)
			}
		})
	}
	got := truncateLeft("路径/very/deep/名前", 10)
	if runewidth.StringWidth(got) != 10 {
		t.Fatalf("display width = %d, want 10 for %q", runewidth.StringWidth(got), got)
	}
	if !strings.HasPrefix(got, "…") || !strings.HasSuffix("路径/very/deep/名前", strings.TrimPrefix(got, "…")) {
		t.Fatalf("truncateLeft did not preserve a cell-safe suffix: %q", got)
	}
}

func TestRenderPathNeverExceedsPaneWidth(t *testing.T) {
	tests := []struct {
		name string
		path pathState
	}{
		{"unavailable", pathState{}},
		{"valid", pathState{value: "/workspace/a/very/long/path", valid: true}},
		{"stale", pathState{value: "/workspace/a/very/long/path", valid: true, stale: true}},
	}
	for _, tt := range tests {
		for _, width := range []int{4, 8, 12} {
			t.Run(fmt.Sprintf("%s/width-%d", tt.name, width), func(t *testing.T) {
				m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: "/workspace"})
				m.Width = width
				line := ansi.Strip(m.renderPath(tt.path))
				if strings.Contains(line, "\n") {
					t.Fatalf("renderPath introduced a physical line break: %q", line)
				}
				if got := runewidth.StringWidth(line); got > width {
					t.Fatalf("renderPath width = %d, want <= %d: %q", got, width, line)
				}
			})
		}
	}
}

func TestViewportCountsExpandedDetailLines(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: "/workspace"})
	m.Height = 8 // heading + up to six content lines + compact footer
	identity := sessionKey{generation: "generation-a", id: "$7"}
	m.expanded[identity] = true
	m.paths[identity] = pathState{value: "/workspace/api", valid: true}
	for i, name := range []string{"vb/api", "vb/web", "vb/worker"} {
		id := fmt.Sprintf("$%d", i+7)
		m.sessions[name] = tmux.SessionInfo{
			ID: id, Generation: "generation-a", Name: name,
		}
	}
	view := m.View()
	if got := len(strings.Split(view, "\n")); got != 8 {
		t.Fatalf("line count = %d, want 8 physical lines:\n%s", got, view)
	}
}

func TestViewportKeepsExpandedParentAndDetailTogether(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: "/workspace"})
	m.Height = 4 // heading + exactly two physical content lines + footer
	key := sessionKey{generation: "generation-a", id: "$8"}
	m.expanded[key] = true
	m.paths[key] = pathState{value: "/workspace/web", valid: true}
	m.sessions["vb/api"] = tmux.SessionInfo{ID: "$7", Generation: "generation-a", Name: "vb/api"}
	m.sessions["vb/web"] = tmux.SessionInfo{ID: "$8", Generation: "generation-a", Name: "vb/web"}
	m.sessions["vb/worker"] = tmux.SessionInfo{ID: "$9", Generation: "generation-a", Name: "vb/worker"}
	m.Cursor = 1

	view := ansi.Strip(m.View())
	if !strings.Contains(view, "vb/web") || !strings.Contains(view, "    /workspace/web") {
		t.Fatalf("selected parent/detail pair was split:\n%s", view)
	}
	if strings.Contains(view, "vb/api") || strings.Contains(view, "vb/worker") {
		t.Fatalf("two-line capacity included an unrelated row:\n%s", view)
	}
}

func TestClickingDetailSelectsParentSession(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb", Root: "/workspace"})
	identity := sessionKey{generation: "generation-a", id: "$7"}
	m.expanded[identity] = true
	m.paths[identity] = pathState{value: "/workspace/api", valid: true}
	m.sessions["vb/api"] = tmux.SessionInfo{ID: "$7", Generation: "generation-a", Name: "vb/api"}
	m.sessions["vb/web"] = tmux.SessionInfo{ID: "$8", Generation: "generation-a", Name: "vb/web"}
	m.Cursor = 1

	mod, _ := m.Update(tea.MouseMsg{
		Y: 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	})
	if got := mod.(Model).Cursor; got != 0 {
		t.Fatalf("detail click selected row %d, want parent row 0", got)
	}
	mod.Update(key("enter"))
	if len(b.switched) != 1 || b.switched[0] != "vb/api" {
		t.Fatalf("Enter after detail click opened %v, want vb/api", b.switched)
	}
}

func TestPollUsesActuallyDisplayedSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.Write("vb", state.Selection{Session: "vb/a"}); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{
		displayed: "vb/b",
		sessions: []tmux.SessionInfo{
			{Name: "vb/a"},
			{Name: "vb/b"},
		},
	}
	m := NewModel(b, Options{WS: "vb", Root: t.TempDir()})
	mod, _ := m.Update(m.fetch()())
	got := mod.(Model)
	if got.effectiveCurrent() != "vb/b" {
		t.Fatalf("effective current = %q, want actual displayed session vb/b", got.effectiveCurrent())
	}

	b.displayed = "vb/a"
	mod, _ = got.Update(got.fetch()())
	if got := mod.(Model).effectiveCurrent(); got != "vb/a" {
		t.Fatalf("effective current after external redirect = %q, want vb/a", got)
	}
}

func TestFetchSurfacesMalformedSelection(t *testing.T) {
	writeMalformedSelection(t, "vb")
	m := NewModel(&fakeBackend{}, Options{WS: "vb"})
	msg := m.fetch()().(rowsMsg)
	if msg.selectionErr == nil || !strings.Contains(msg.selectionErr.Error(), "unmarshal") {
		t.Fatalf("selection error = %v, want malformed state error", msg.selectionErr)
	}
}

func TestMalformedSelectionDoesNotFreezeSessionRowsOrAlerts(t *testing.T) {
	writeMalformedSelection(t, "vb")
	b := &fakeBackend{sessions: []tmux.SessionInfo{{Name: "vb/api", Bell: true}}}
	m := NewModel(b, Options{WS: "vb"})

	updated, _ := m.Update(m.fetch()())
	got := updated.(Model)
	rows := got.rows()
	if len(rows) != 1 || rows[0].name != "vb/api" || !rows[0].bell {
		t.Fatalf("rows = %+v, want fresh ringing vb/api despite malformed selection", rows)
	}
	if len(b.alerts) != 1 || !b.alerts[0] {
		t.Fatalf("alerts = %v, want newly observed bell to raise workspace alert", b.alerts)
	}
	if b.rings != 1 {
		t.Fatalf("rings = %d, want one newly observed bell notification", b.rings)
	}
	if footer := got.footer(); !strings.Contains(footer, "unmarshal") {
		t.Fatalf("footer = %q, want malformed selection error", footer)
	}
}

func TestNewTermSurfacesMalformedSelection(t *testing.T) {
	writeMalformedSelection(t, "vb")
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb", Root: "/root"})
	updated, _ := m.Update(key("n"))
	got := updated.(Model)
	if len(b.newTermCalls) != 0 {
		t.Fatalf("NewTerm called despite unreadable selection: %v", b.newTermCalls)
	}
	if !strings.Contains(got.footer(), "unmarshal") {
		t.Fatalf("footer = %q, want malformed state error", got.footer())
	}
}

func TestRowsScopedToWorkspace(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb"},
		{Name: "vb/x"},
		{Name: "vb·term·1"},
		{Name: "vbextra"},
		{Name: "other/y"},
		{Name: "wrap-home"},
	}})
	sm := mod.(Model)
	rows := sm.rows()
	if len(rows) != 3 {
		t.Fatalf("rows = %+v, want 3 (vb, vb/x, vb·term·1)", rows)
	}
	// Assert that vbextra (shares ws prefix but lacks delimiter) is excluded.
	for _, r := range rows {
		if r.name == "vbextra" {
			t.Errorf("vbextra should not be included in rows: %+v", rows)
		}
	}
}

func TestRowsKeepCreationOrderWhenAttentionChanges(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{ID: "$9", Name: "vb/z", Created: 100, Activity: 100},
		{ID: "$10", Name: "vb/a", Created: 101, Activity: 100},
		{ID: "$11", Name: "vb/m", Created: 102, Activity: 100},
	}})
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{ID: "$9", Name: "vb/z", Created: 100, Activity: 100, Bell: true},
		{ID: "$10", Name: "vb/a", Created: 101, Activity: 200},
		{ID: "$11", Name: "vb/m", Created: 102, Activity: 100},
	}})
	sm := mod.(Model)
	rows := sm.rows()
	if len(rows) != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	for i, want := range []string{"vb/z", "vb/a", "vb/m"} {
		if rows[i].name != want {
			t.Fatalf("rows changed creation order for bell/activity: %+v", rows)
		}
	}
	if !rows[0].bell || !rows[1].activity {
		t.Fatalf("creation ordering lost attention flags: %+v", rows)
	}
}

func TestRowsBreakCreationTimeTiesByNumericSessionID(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{ID: "$10", Name: "vb/a", Created: 100},
		{ID: "$9", Name: "vb/z", Created: 100},
	}})
	rows := mod.(Model).rows()
	if len(rows) != 2 || rows[0].name != "vb/z" || rows[1].name != "vb/a" {
		t.Fatalf("same-second rows = %+v, want numeric ID order [vb/z vb/a]", rows)
	}
}

func TestBellAndActivityIcons(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb/a", Activity: 100},
		{Name: "vb/b", Activity: 100, Bell: true},
	}})
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb/a", Activity: 200},
		{Name: "vb/b", Activity: 100, Bell: true},
	}})
	v := mod.View()
	if !strings.Contains(v, "🔔") {
		t.Errorf("missing bell icon in view:\n%s", v)
	}
	if !strings.Contains(v, "!") {
		t.Errorf("missing activity icon in view:\n%s", v)
	}
}

func TestRenderRowUsesCompactMarkers(t *testing.T) {
	tests := []struct {
		name string
		row  row
		want string
	}{
		{name: "ordinary", row: row{name: "repo"}, want: "  › repo"},
		{name: "current", row: row{name: "repo", current: true}, want: "▸ › repo"},
		{name: "expanded", row: row{name: "repo", current: true, expanded: true}, want: "▸ ⌄ repo"},
		{name: "activity", row: row{name: "repo", activity: true}, want: "  ! › repo"},
		{name: "bell", row: row{name: "repo", bell: true}, want: "  🔔 › repo"},
		{name: "bell wins", row: row{name: "repo", bell: true, activity: true}, want: "  🔔 › repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderRow(tt.row, 0); got != tt.want {
				t.Fatalf("renderRow = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEnterSwitches pins that Enter switches the middle pane via
// ShowInMiddle. It is a structural guarantee (not something asserted at
// runtime) that this never moves pane focus: fakeBackend's ShowInMiddle
// has no select-pane concept at all — terms literally has no method to
// call for it — unlike the tree pane, which uses SwitchMiddle.
func TestEnterSwitches(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{ID: "$7", Generation: "generation", Name: "vb/x"}}})
	mod, _ = mod.Update(key("enter"))
	if len(b.switched) != 1 || b.switched[0] != "vb/x" {
		t.Fatalf("switched = %v", b.switched)
	}
	_ = mod
}

// TestNewTermCreatesAndSwitches pins the no-selection case: with no
// state.Selection on disk, n creates a scratch terminal at the workspace
// root (not bound to anything) and shows it.
func TestNewTermCreatesAndSwitches(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	b := &fakeBackend{newTermName: "vb·term·2"}
	m := NewModel(b, Options{WS: "vb", Root: "/root", Cmd: "vim"})
	var mod tea.Model = m
	var cmd tea.Cmd
	mod, cmd = mod.Update(key("n"))
	if len(b.ensureCalls) != 0 {
		t.Fatalf("EnsureEntrySession should not be called with no selection: %v", b.ensureCalls)
	}
	if len(b.newTermCalls) != 1 || b.newTermCalls[0] != "/root|vim" {
		t.Fatalf("NewTerm calls = %v", b.newTermCalls)
	}
	if len(b.switched) != 1 || b.switched[0] != "vb·term·2" {
		t.Fatalf("switched = %v", b.switched)
	}
	if cmd == nil {
		t.Fatal("n should return an immediate refresh cmd")
	}
	if _, ok := cmd().(rowsMsg); !ok {
		t.Fatalf("refresh cmd should yield rowsMsg, got %T", cmd())
	}
	_ = mod
}

// TestNewTermBindsSelectionSession pins the selection-aware n path: when
// the tree's current selection names a session that isn't alive yet (a
// row picked before any terminal existed for it), n binds THAT session
// via EnsureEntrySession — using the selection's path and the pane's
// default cmd — instead of opening an unrelated scratch terminal at root.
func TestNewTermBindsSelectionSession(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.Write("vb", state.Selection{Entry: "repo1", Session: "vb/repo1", Path: "/repo1"}); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb", Root: "/root", Cmd: "vim"})
	var mod tea.Model = m
	var cmd tea.Cmd
	mod, cmd = mod.Update(key("n"))
	if len(b.ensureCalls) != 1 || b.ensureCalls[0] != "vb/repo1|/repo1|vim" {
		t.Fatalf("ensureCalls = %v", b.ensureCalls)
	}
	if len(b.newTermCalls) != 0 {
		t.Fatalf("NewTerm should not be called when binding a selection session: %v", b.newTermCalls)
	}
	if len(b.switched) != 1 || b.switched[0] != "vb/repo1" {
		t.Fatalf("switched = %v", b.switched)
	}
	if cmd == nil {
		t.Fatal("n should return an immediate refresh cmd")
	}
	_ = mod
}

// TestNewTermUsesSelectionPathWhenSessionExists pins that when the
// selection's bound session is ALREADY alive (present among current
// sessions), n falls through to creating a scratch terminal — but at the
// SELECTION's path, not the workspace root.
func TestNewTermUsesSelectionPathWhenSessionExists(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.Write("vb", state.Selection{Entry: "repo1", Session: "vb/repo1", Path: "/repo1"}); err != nil {
		t.Fatal(err)
	}
	b := &fakeBackend{newTermName: "vb·term·1"}
	m := NewModel(b, Options{WS: "vb", Root: "/root", Cmd: "vim"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{Name: "vb/repo1"}}})
	mod, _ = mod.Update(key("n"))
	if len(b.ensureCalls) != 0 {
		t.Fatalf("EnsureEntrySession should not be called when the bound session already exists: %v", b.ensureCalls)
	}
	if len(b.newTermCalls) != 1 || b.newTermCalls[0] != "/repo1|vim" {
		t.Fatalf("NewTerm calls = %v, want dir = selection path", b.newTermCalls)
	}
	_ = mod
}

// TestQuitDetachesFromTerms pins that q detaches from this pane too
// (mirroring the tree's q), but only in normal mode — while renaming, q
// is a literal rune that must not detach.
func TestQuitDetachesFromTerms(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		Name: "vb·term·1", Kind: tmux.SessionKindScratch,
	}}})
	mod.Update(key("q"))
	if !b.detached {
		t.Fatal("q in normal mode should detach")
	}

	b2 := &fakeBackend{}
	m2 := NewModel(b2, Options{WS: "vb"})
	var mod2 tea.Model = m2
	mod2, _ = mod2.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		Name: "vb·term·1", Kind: tmux.SessionKindScratch,
	}}})
	mod2, _ = mod2.Update(key("r")) // enter rename mode
	mod2, _ = mod2.Update(key("q"))
	sm2 := mod2.(Model)
	if b2.detached {
		t.Error("q while renaming should not detach")
	}
	if string(sm2.renameBuf) != "q" {
		t.Errorf("q while renaming should append to the rename buffer, got %q", string(sm2.renameBuf))
	}
}

// TestShutdownConfirmFlow pins the Q shutdown-confirm flow: Q shows a
// confirm prompt without touching anything, y calls
// backend.ShutdownWorkspace, and any other key cancels without calling it.
func TestShutdownConfirmFlow(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
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
	m := NewModel(b, Options{WS: "vb"})
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
// via errText.
func TestShutdownErrorSurfaced(t *testing.T) {
	b := &fakeBackend{shutdownErr: fmt.Errorf("kill vb/x: boom")}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(key("Q"))
	mod, _ = mod.Update(key("y"))
	if v := mod.View(); !strings.Contains(v, "boom") {
		t.Errorf("expected shutdown error in view:\n%s", v)
	}
}

// TestShutdownNotReachableWhileRenaming pins that Q while renaming is a
// literal rune appended to the buffer, not the shutdown shortcut —
// mirroring how q behaves during rename.
func TestShutdownNotReachableWhileRenaming(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		Name: "vb·term·1", Kind: tmux.SessionKindScratch,
	}}})
	mod, _ = mod.Update(key("r")) // enter rename mode
	mod, _ = mod.Update(key("Q"))
	sm := mod.(Model)
	if b.shutdownCalls != 0 {
		t.Error("Q while renaming should not trigger shutdown")
	}
	if sm.ConfirmShutdown {
		t.Error("Q while renaming should not arm the shutdown confirm")
	}
	if string(sm.renameBuf) != "Q" {
		t.Errorf("Q while renaming should append to the rename buffer, got %q", string(sm.renameBuf))
	}
}

func TestKillRejectsNonScratchSessionsBeforeConfirmation(t *testing.T) {
	for _, info := range []tmux.SessionInfo{
		{Name: "vb", Kind: tmux.SessionKindEntry},
		{Name: "vb/repo", Kind: tmux.SessionKindEntry},
		{Name: "vb·diff", Kind: tmux.SessionKindDiff},
		{
			Name: "vb·term·renamed-entry", Kind: tmux.SessionKindEntry,
			EntryName: "vb/repo",
		},
	} {
		t.Run(info.Name, func(t *testing.T) {
			b := &fakeBackend{}
			m := NewModel(b, Options{WS: "vb"})
			var mod tea.Model = m
			info.ID = "$7"
			info.Generation = "generation"
			info.Created = 100
			mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{info}})
			mod, _ = mod.Update(key("x"))
			got := mod.(Model)
			if got.ConfirmKill {
				t.Fatal("x armed kill confirmation for a non-scratch session")
			}
			if len(b.killed) != 0 {
				t.Fatalf("x called the kill backend for a non-scratch session: %v", b.killed)
			}
			if !strings.Contains(got.footer(), "only scratch terminals can be killed") {
				t.Fatalf("footer = %q, want non-scratch kill refusal", got.footer())
			}
		})
	}
}

func TestKillConfirm(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		ID: "$7", Generation: "generation", Name: "vb·term·1", Kind: tmux.SessionKindScratch,
	}}})
	mod, _ = mod.Update(key("x"))
	if len(b.killed) != 0 {
		t.Fatal("killed before confirm")
	}
	if v := mod.View(); !strings.Contains(v, "kill vb·term·1? y/n") {
		t.Errorf("confirm prompt should name target vb·term·1, view:\n%s", v)
	}
	mod.Update(key("y"))
	if len(b.killed) != 1 || b.killed[0] != "vb·term·1" {
		t.Errorf("killed = %v", b.killed)
	}
	if b.scratchKillCalls != 1 {
		t.Errorf("scratch kill calls = %d, want 1", b.scratchKillCalls)
	}
	if b.killID != "$7" {
		t.Errorf("stable kill ID = %q, want $7", b.killID)
	}
	if b.killGeneration != "generation" {
		t.Errorf("kill generation = %q, want generation", b.killGeneration)
	}
}

// TestKillConfirmSurvivesRowChurn pins I2: the session captured at "x"
// time is the one killed on "y", even if a new row changes its index.
func TestKillConfirmSurvivesRowChurn(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{ID: "$7", Generation: "generation", Name: "vb·term·a", Kind: tmux.SessionKindScratch, Created: 100, Activity: 100},
		{ID: "$9", Generation: "generation", Name: "vb·term·b", Kind: tmux.SessionKindScratch, Created: 200, Activity: 100},
	}})
	mod, _ = mod.Update(key("j")) // cursor -> vb·term·b (second-created)
	sm := mod.(Model)
	if rows := sm.rows(); rows[sm.Cursor].name != "vb·term·b" {
		t.Fatalf("setup: cursor should be on vb·term·b, rows=%+v cursor=%d", rows, sm.Cursor)
	}
	mod, _ = mod.Update(key("x")) // confirm — captures vb·term·b

	// A newly observed session whose creation time falls between the original
	// rows pushes the target down, leaving vb·term·aa at the old cursor index.
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{ID: "$7", Generation: "generation", Name: "vb·term·a", Kind: tmux.SessionKindScratch, Created: 100, Activity: 100},
		{ID: "$8", Generation: "generation", Name: "vb·term·aa", Kind: tmux.SessionKindScratch, Created: 150, Activity: 100},
		{ID: "$10", Generation: "new-generation", Name: "vb·term·b", Kind: tmux.SessionKindScratch, Created: 300, Activity: 100},
	}})
	sm = mod.(Model)
	if rows := sm.rows(); rows[sm.Cursor].name != "vb·term·aa" {
		t.Fatalf("setup: insertion should leave vb·term·aa at the old cursor index, rows=%+v cursor=%d", rows, sm.Cursor)
	}

	mod.Update(key("y"))
	if len(b.killed) != 1 || b.killed[0] != "vb·term·b" {
		t.Errorf("killed = %v, want [vb·term·b] (the session captured at x-time)", b.killed)
	}
	if b.killID != "$9" {
		t.Errorf("kill ID = %q, want original $9 rather than replacement", b.killID)
	}
	if b.killGeneration != "generation" {
		t.Errorf("kill generation = %q, want original generation", b.killGeneration)
	}
}

func TestViewHasTerminalsHeadingAndCompactFooter(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{
		WS:   "vb",
		Keys: config.Keys{FocusTerms: "M-3"},
	})
	view := ansi.Strip(m.View())
	for _, want := range []string{"Terminals (⌥3)", "h help · q detach · Q shutdown"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "terms: ↵ open") {
		t.Fatalf("old permanent help remains:\n%s", view)
	}
}

func TestTermsHelpCloseKeysDoNotDetach(t *testing.T) {
	for _, closeKey := range []tea.KeyMsg{
		key("h"),
		{Type: tea.KeyEsc},
		key("q"),
	} {
		t.Run(closeKey.String(), func(t *testing.T) {
			b := &fakeBackend{}
			m := NewModel(b, Options{WS: "vb"})
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

func TestTermsHelpKeepsActionsInertAndPollingLive(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	mod, _ := m.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{ID: "$7", Generation: "generation", Name: "vb/a"},
		{ID: "$8", Generation: "generation", Name: "vb/b"},
	}})
	got := mod.(Model)
	got.Cursor = 1
	keyA := sessionKey{generation: "generation", id: "$7"}
	got.expanded[keyA] = true
	got.paths[keyA] = pathState{value: "/workspace/a", valid: true}
	mod, _ = got.Update(key("h"))

	mod, _ = mod.Update(key("k"))
	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 1})
	mod, _ = mod.Update(key("Q"))
	got = mod.(Model)
	if got.Cursor != 1 || !got.expanded[keyA] || got.ConfirmShutdown {
		t.Fatalf("Help mutated actions: cursor=%d expanded=%v shutdown=%v", got.Cursor, got.expanded[keyA], got.ConfirmShutdown)
	}

	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		ID: "$9", Generation: "generation", Name: "vb/c",
	}}})
	if _, ok := mod.(Model).sessions["vb/c"]; !ok {
		t.Fatal("session poll was ignored while Help was open")
	}
}

func TestTermsHelpFitsSmallPaneHeights(t *testing.T) {
	for _, height := range []int{1, 2} {
		t.Run(fmt.Sprintf("height_%d", height), func(t *testing.T) {
			m := NewModel(&fakeBackend{}, Options{WS: "vb"})
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

func TestTermsRenameAndConfirmationKeepPriorityOverHelp(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	mod, _ := m.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		ID: "$7", Generation: "generation", Name: "vb·term·1",
		Kind: tmux.SessionKindScratch,
	}}})
	mod, _ = mod.Update(key("r"))
	mod, _ = mod.Update(key("h"))
	got := mod.(Model)
	if got.helpOpen || string(got.renameBuf) != "h" {
		t.Fatalf("rename should consume h: help=%v buffer=%q", got.helpOpen, got.renameBuf)
	}

	m = NewModel(b, Options{WS: "vb"})
	mod, _ = m.Update(key("Q"))
	mod, _ = mod.Update(key("h"))
	got = mod.(Model)
	if got.ConfirmShutdown || got.helpOpen {
		t.Fatalf("h should cancel confirmation without opening Help: %+v", got.Nav)
	}
}

func TestViewNeverEmitsLineWiderThanPane(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: "/workspace"})
	m.Width = 55
	m.Height = 8
	key := sessionKey{generation: "generation-a", id: "$7"}
	m.expanded[key] = true
	m.paths[key] = pathState{value: "/workspace/api", valid: true}
	m.sessions["vb/api"] = tmux.SessionInfo{
		ID: "$7", Generation: "generation-a", Name: "vb/api",
	}

	for i, line := range strings.Split(m.View(), "\n") {
		plain := ansi.Strip(line)
		if width := runewidth.StringWidth(plain); width > m.Width {
			t.Fatalf("line %d width = %d, want <= %d: %q", i, width, m.Width, plain)
		}
	}
}

func TestViewPinsCompactFooterToBottom(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(tea.WindowSizeMsg{Height: 20})
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb/a"},
		{Name: "vb/b"},
	}})
	v := mod.View()
	lines := strings.Split(v, "\n")
	if len(lines) != 20 {
		t.Fatalf("line count = %d, want 20 (pinned to pane height):\n%s", len(lines), v)
	}
	if !strings.Contains(lines[19], "h help") {
		t.Errorf("last line = %q, want the compact action footer", lines[19])
	}
	if !strings.Contains(lines[0], "Terminals") {
		t.Errorf("line 0 = %q, want pane heading", lines[0])
	}
}

func TestViewNoHeightFallsBackToFlow(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb/a"},
	}})
	v := mod.View()
	lines := strings.Split(v, "\n")
	if !strings.Contains(lines[0], "Terminals") {
		t.Errorf("line 0 = %q, want pane heading", lines[0])
	}
	if !strings.Contains(lines[1], "vb/a") {
		t.Errorf("line 1 = %q, want first row after heading", lines[1])
	}
	last := lines[len(lines)-1]
	if !strings.Contains(last, "h help") {
		t.Errorf("last line = %q, want compact action footer", last)
	}
}

func TestCurrentRowPrefix(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{
		sessions: []tmux.SessionInfo{
			{Name: "vb/a", Activity: 100},
			{Name: "vb/b", Activity: 100},
		},
		current: "vb/b",
	})
	v := mod.View()
	// The current row (vb/b) should have the ▸ prefix.
	if !strings.Contains(v, "▸ › vb/b") {
		t.Errorf("current row should start with ▸ prefix, view:\n%s", v)
	}
	// A non-current row should have "  " prefix (two spaces).
	if !strings.Contains(v, "  › vb/a") {
		t.Errorf("non-current row should have double-space prefix, view:\n%s", v)
	}
}

// TestRenameFlow pins the end-to-end rename flow: r captures the target
// row at press-time, typed runes accumulate in the buffer, and enter
// calls backend.RenameTerm and immediately refreshes rows.
func TestRenameFlow(t *testing.T) {
	b := &fakeBackend{renameName: "vb·term·logs"}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		ID: "$7", Generation: "0123456789abcdef0123456789abcdef", Name: "vb·term·1",
		Kind: tmux.SessionKindScratch,
	}}})

	mod, _ = mod.Update(key("r"))
	sm := mod.(Model)
	if !sm.renaming {
		t.Fatal("r on a ·term· row should enter rename mode")
	}
	if v := mod.View(); !strings.Contains(v, " rename: ▌ (enter/esc) ") {
		t.Errorf("empty rename buffer should show in footer, view:\n%s", v)
	}

	mod, _ = mod.Update(key("logs"))
	if v := mod.View(); !strings.Contains(v, " rename: logs▌ (enter/esc) ") {
		t.Errorf("typed rename buffer should show in footer, view:\n%s", v)
	}

	var cmd tea.Cmd
	mod, cmd = mod.Update(key("enter"))
	if len(b.renameCalls) != 1 ||
		b.renameCalls[0] != "vb·term·1|$7|0123456789abcdef0123456789abcdef|logs" {
		t.Fatalf("renameCalls = %v", b.renameCalls)
	}
	sm = mod.(Model)
	if sm.renaming {
		t.Error("enter should exit rename mode")
	}
	if cmd == nil {
		t.Fatal("enter should return an immediate refresh cmd")
	}
	if _, ok := cmd().(rowsMsg); !ok {
		t.Fatalf("refresh cmd should yield rowsMsg, got %T", cmd())
	}
}

// TestRenameUpdatesLocalDisplay pins that a successful rename of the
// session currently shown locally (m.display) follows it to the new
// name, so this pane's ▸ doesn't point at a session that no longer
// exists.
func TestRenameUpdatesLocalDisplay(t *testing.T) {
	b := &fakeBackend{renameName: "vb·term·logs"}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		Name: "vb·term·1", Kind: tmux.SessionKindScratch,
	}}})
	mod, _ = mod.Update(key("enter")) // display -> vb·term·1
	sm := mod.(Model)
	if sm.display != "vb·term·1" {
		t.Fatalf("setup: display = %q, want vb·term·1", sm.display)
	}

	mod, _ = mod.Update(key("r"))
	mod, _ = mod.Update(key("logs"))
	mod, _ = mod.Update(key("enter"))
	sm = mod.(Model)
	if sm.display != "vb·term·logs" {
		t.Errorf("display should follow the rename, got %q", sm.display)
	}
}

// TestRenameEscCancels pins that esc exits rename mode without calling
// RenameTerm.
func TestRenameEscCancels(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		Name: "vb·term·1", Kind: tmux.SessionKindScratch,
	}}})
	mod, _ = mod.Update(key("r"))
	mod, _ = mod.Update(key("logs"))
	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyEsc})
	sm := mod.(Model)
	if sm.renaming {
		t.Error("esc should cancel rename mode")
	}
	if len(b.renameCalls) != 0 {
		t.Errorf("esc should not call RenameTerm: %v", b.renameCalls)
	}
}

// TestRenameNonTermRowErrors pins that r on a row without the ·term·
// prefix refuses with an error, without entering rename mode.
func TestRenameNonTermRowErrors(t *testing.T) {
	for _, info := range []tmux.SessionInfo{
		{Name: "vb/repo1", Kind: tmux.SessionKindEntry},
		{
			Name: "vb·term·renamed-entry", Kind: tmux.SessionKindEntry,
			EntryName: "vb/repo1",
		},
	} {
		t.Run(info.Name, func(t *testing.T) {
			b := &fakeBackend{}
			m := NewModel(b, Options{WS: "vb"})
			var mod tea.Model = m
			mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{info}})
			mod, _ = mod.Update(key("r"))
			sm := mod.(Model)
			if sm.renaming {
				t.Error("r on a non-scratch row should not enter rename mode")
			}
			if v := mod.View(); !strings.Contains(v, "only scratch terminals can be renamed") {
				t.Errorf("expected refusal error, view:\n%s", v)
			}
		})
	}
}

// TestRenameSuspendsNavigation pins that while renaming, keys that
// otherwise navigate/act (j/k/n/x/enter... as runes, not the literal
// enter key) are consumed by the rename buffer instead of moving the
// cursor, killing, or creating a new terminal.
func TestRenameSuspendsNavigation(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb·term·1", Kind: tmux.SessionKindScratch},
		{Name: "vb·term·2", Kind: tmux.SessionKindScratch},
	}})
	mod, _ = mod.Update(key("r"))
	sm := mod.(Model)
	cursorBefore := sm.Cursor

	mod, _ = mod.Update(key("j"))
	sm = mod.(Model)
	if sm.Cursor != cursorBefore {
		t.Errorf("j while renaming should not move cursor: cursor=%d want %d", sm.Cursor, cursorBefore)
	}

	mod, _ = mod.Update(key("x"))
	sm = mod.(Model)
	if sm.ConfirmKill {
		t.Error("x while renaming should not trigger kill-confirm")
	}

	mod, _ = mod.Update(key("n"))
	if len(b.newTermCalls) != 0 {
		t.Errorf("n while renaming should not create a new term: %v", b.newTermCalls)
	}

	sm = mod.(Model)
	if !sm.renaming {
		t.Fatal("should still be in rename mode")
	}
	if string(sm.renameBuf) != "jxn" {
		t.Errorf("renameBuf = %q, want %q (all keys typed into buffer)", string(sm.renameBuf), "jxn")
	}
}

// TestRenameBackspace pins that backspace removes the last rune from the
// rename buffer.
func TestRenameBackspace(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{{
		Name: "vb·term·1", Kind: tmux.SessionKindScratch,
	}}})
	mod, _ = mod.Update(key("r"))
	mod, _ = mod.Update(key("logs"))
	mod, _ = mod.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	sm := mod.(Model)
	if string(sm.renameBuf) != "log" {
		t.Errorf("renameBuf = %q, want %q", string(sm.renameBuf), "log")
	}
}

// TestMouseClickMovesCursorNoBackendCalls pins the commit-2 fix: a left
// click on an activatable row only moves the cursor — it never calls the
// backend. A click-then-Enter flow (below) proves selection-by-mouse
// still reaches activation via Enter.
func TestMouseClickMovesCursorNoBackendCalls(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb/a", Activity: 100},
		{Name: "vb/b", Activity: 100},
	}})
	// Rows sort by creation fallback here: vb/a(0), vb/b(1). The heading
	// occupies physical line 0, so vb/b is physical line 2.
	mod, _ = mod.Update(tea.MouseMsg{Y: 2, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	sm := mod.(Model)
	if sm.Cursor != 1 {
		t.Fatalf("cursor = %d, want 1", sm.Cursor)
	}
	if len(b.switched) != 0 || len(b.killed) != 0 {
		t.Fatalf("click should make zero backend calls: switched=%v killed=%v", b.switched, b.killed)
	}

	mod.Update(key("enter"))
	if len(b.switched) != 1 || b.switched[0] != "vb/b" {
		t.Fatalf("Enter after click-select should activate: switched=%v", b.switched)
	}
}

// TestMouseWheelMovesCursor pins the wheel-scroll fix: wheel up/down steps
// the cursor by one row, clamped at both ends. bubbletea v1.3.10 reports
// wheel events with Action == MouseActionPress (its zero value) — see
// tree.TestMouseWheelMovesCursor for the source finding — so dispatch here
// is on Button alone, not gated on Action.
func TestMouseWheelMovesCursor(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb/a", Activity: 100},
		{Name: "vb/b", Activity: 100},
		{Name: "vb/c", Activity: 100},
	}})

	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if sm := mod.(Model); sm.Cursor != 0 {
		t.Fatalf("wheel up at top should clamp to 0, got %d", sm.Cursor)
	}

	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if sm := mod.(Model); sm.Cursor != 1 {
		t.Fatalf("wheel down should move cursor to 1, got %d", sm.Cursor)
	}
	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if sm := mod.(Model); sm.Cursor != 2 {
		t.Fatalf("wheel down should move cursor to 2, got %d", sm.Cursor)
	}
	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if sm := mod.(Model); sm.Cursor != 2 {
		t.Fatalf("wheel down at bottom should clamp to last row (2), got %d", sm.Cursor)
	}
	if len(b.switched) != 0 || len(b.killed) != 0 {
		t.Fatalf("wheel should make zero backend calls: switched=%v killed=%v", b.switched, b.killed)
	}

	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if sm := mod.(Model); sm.Cursor != 1 {
		t.Fatalf("wheel up should move cursor to 1, got %d", sm.Cursor)
	}
}

func TestViewKeepsCursorAndKillTargetInsideViewport(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb", Root: "/root"})
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("vb/%02d", i)
		m.sessions[name] = tmux.SessionInfo{Name: name}
	}
	m.Height = 8
	m.Cursor = len(m.rows()) - 1
	if view := m.View(); !strings.Contains(view, "vb/09") || strings.Contains(view, "vb/00") {
		t.Fatalf("viewport did not keep the last cursor visible:\n%s", view)
	}

	m.ArmKill("vb/00", "$7", "generation", "")
	if view := m.View(); !strings.Contains(view, "vb/00") {
		t.Fatalf("viewport did not keep the confirmation target visible:\n%s", view)
	}
}

func TestViewportTranslatesMouseRows(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: "/root"})
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("vb/%02d", i)
		m.sessions[name] = tmux.SessionInfo{Name: name}
	}
	m.Height = 8
	m.Cursor = len(m.rows()) - 1
	mod, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 1})
	if got := mod.(Model).Cursor; got != len(m.rows())-6 {
		t.Fatalf("cursor = %d, want first visible row %d", got, len(m.rows())-6)
	}
}

func TestViewportIgnoresClicksBelowVisibleRows(t *testing.T) {
	m := NewModel(&fakeBackend{}, Options{WS: "vb", Root: "/root"})
	for i := 0; i < 10; i++ {
		name := fmt.Sprintf("vb/%02d", i)
		m.sessions[name] = tmux.SessionInfo{Name: name}
	}
	m.Height = 8
	m.Cursor = len(m.rows()) - 1
	before := m.Cursor
	mod, _ := m.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 7})
	if got := mod.(Model).Cursor; got != before {
		t.Fatalf("help click moved cursor from %d to hidden row %d", before, got)
	}
}

// TestMouseIgnoredDuringConfirm pins that mouse events (click and wheel)
// are ignored while a kill-confirm prompt is active, mirroring the guard
// already in place for keys.
func TestMouseIgnoredDuringConfirm(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{ID: "$7", Name: "vb·term·a", Kind: tmux.SessionKindScratch, Activity: 100},
		{ID: "$8", Name: "vb·term·b", Kind: tmux.SessionKindScratch, Activity: 100},
	}})
	mod, _ = mod.Update(key("x")) // confirmKill armed on vb·term·a (cursor 0)
	before := mod.(Model).Cursor
	mod, _ = mod.Update(tea.MouseMsg{Y: 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if sm := mod.(Model); sm.Cursor != before || !sm.ConfirmKill {
		t.Errorf("click during kill-confirm should be ignored: cursor=%d confirmKill=%v", sm.Cursor, sm.ConfirmKill)
	}
	mod, _ = mod.Update(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if sm := mod.(Model); sm.Cursor != before || !sm.ConfirmKill {
		t.Errorf("wheel during kill-confirm should be ignored: cursor=%d confirmKill=%v", sm.Cursor, sm.ConfirmKill)
	}
}

// TestMouseIgnoredDuringRename pins that mouse events are ignored while
// renaming — a click or wheel must not move the cursor out from under the
// row captured at "r" time.
func TestMouseIgnoredDuringRename(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb·term·1", Kind: tmux.SessionKindScratch},
		{Name: "vb·term·2", Kind: tmux.SessionKindScratch},
	}})
	mod, _ = mod.Update(key("r")) // rename mode on vb·term·1 (cursor 0)
	before := mod.(Model).Cursor
	mod, _ = mod.Update(tea.MouseMsg{Y: 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	sm := mod.(Model)
	if sm.Cursor != before || !sm.renaming {
		t.Errorf("click while renaming should be ignored: cursor=%d renaming=%v", sm.Cursor, sm.renaming)
	}
}

// TestEnterDisplayThenStateChangeFollowsState pins I5a: Enter switches
// the ▸ marker locally (m.display) even though it doesn't persist
// state.Selection; a later rowsMsg carrying a CHANGED state current (the
// tree made its own selection) supersedes that local override.
func TestEnterDisplayThenStateChangeFollowsState(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	sessions := []tmux.SessionInfo{
		{Name: "vb/a", Activity: 100},
		{Name: "vb/b", Activity: 100},
		{Name: "vb/c", Activity: 100},
	}
	mod, _ = mod.Update(rowsMsg{sessions: sessions, current: "vb/a"})

	mod, _ = mod.Update(key("j")) // cursor -> vb/b
	mod, _ = mod.Update(key("enter"))
	if v := mod.View(); !strings.Contains(v, "▸ › vb/b") {
		t.Fatalf("Enter should move ▸ to vb/b via local display, view:\n%s", v)
	}

	// The tree makes a new selection (state current changes vb/a -> vb/c):
	// it supersedes the local display override.
	mod, _ = mod.Update(rowsMsg{sessions: sessions, current: "vb/c"})
	v := mod.View()
	if !strings.Contains(v, "▸ › vb/c") {
		t.Errorf("▸ should follow the new state current, view:\n%s", v)
	}
	if strings.Contains(v, "▸ › vb/b") {
		t.Errorf("local display override should be cleared by new state current, view:\n%s", v)
	}
}

// successorRow names where the terminal pane lands when the row being
// killed is the one on screen.
func TestSuccessorRow(t *testing.T) {
	rows := []row{{name: "a"}, {name: "b"}, {name: "c"}}
	if got := successorRow(rows, 0); got != "b" {
		t.Errorf("killing the first row should fall to the next: %q", got)
	}
	if got := successorRow(rows, 1); got != "c" {
		t.Errorf("killing a middle row should fall to the next: %q", got)
	}
	// Killing the last row has nothing below it, so it falls upward.
	if got := successorRow(rows, 2); got != "b" {
		t.Errorf("killing the last row should fall to the previous: %q", got)
	}
	// Killing the only row leaves nothing to land on.
	if got := successorRow([]row{{name: "only"}}, 0); got != "" {
		t.Errorf("a sole row has no successor, got %q", got)
	}
}

// x must capture the successor alongside the target from the same row
// snapshot because rows can change on later ticks.
func TestKillCapturesSuccessorAtArmTime(t *testing.T) {
	b := &fakeBackend{sessions: []tmux.SessionInfo{
		{ID: "$7", Name: "vb·term·a", Kind: tmux.SessionKindScratch},
		{ID: "$8", Name: "vb·term·b", Kind: tmux.SessionKindScratch},
		{ID: "$9", Name: "vb·term·c", Kind: tmux.SessionKindScratch},
	}}
	m := NewModel(b, Options{WS: "vb", Root: "/root"})
	m2, _ := m.Update(rowsMsg{sessions: b.sessions})
	mm := m2.(Model)
	mm.Cursor = 0
	m3, _ := mm.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	armed := m3.(Model)
	if armed.ConfirmTarget == "" || armed.ConfirmSuccessor == "" {
		t.Fatalf("x should arm both target and successor, got %+v", armed.Nav)
	}
	if armed.ConfirmSuccessor == armed.ConfirmTarget {
		t.Errorf("successor must differ from the target: %q", armed.ConfirmSuccessor)
	}
}

// A tmux hiccup must not read as "no sessions". Both panes poll every 2s,
// and treating a transient failure as an empty list wipes every row, every
// 🔔, and every "!". The alert is the expensive part: dropping it clears
// the tab title and resets m.alerted, so the NEXT good poll sees the bell
// as newly raised and rings the terminal a second time for something the
// user already acknowledged.
func TestTransientSessionsFailureKeepsRowsAndAlert(t *testing.T) {
	b := &fakeBackend{}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(rowsMsg{sessions: []tmux.SessionInfo{
		{Name: "vb/api", Bell: true},
		{Name: "vb\u00b7term\u00b71"},
	}})
	before := mod.(Model)
	if len(before.rows()) != 2 || !before.alerted {
		t.Fatalf("precondition: %d rows, alerted=%v — want 2 rows and a raised alert",
			len(before.rows()), before.alerted)
	}

	// The poll itself fails, exactly as it does when the session server is
	// briefly unreachable. Drive the real fetch path, not a hand-built msg.
	b.sessionsErr = tmux.ErrNoServer
	mod, _ = mod.Update(before.fetch()())
	after := mod.(Model)

	if len(after.rows()) != 2 {
		t.Errorf("rows = %d, want 2 — one failed poll erased the session list", len(after.rows()))
	}
	for _, r := range after.rows() {
		if r.name == "vb/api" && !r.bell {
			t.Error("the bell on vb/api was cleared by a failed poll, not by the user looking at it")
		}
	}
	if !after.alerted {
		t.Error("a failed poll withdrew the workspace alert")
	}
	if len(b.alerts) != 1 || b.alerts[0] != true {
		t.Errorf("alerts = %v, want exactly [true] — a false here re-rings the terminal on the next good poll", b.alerts)
	}

	// Ignoring a failed poll must not wedge the pane on stale data: the
	// next poll that succeeds still gets through, bell now genuinely gone.
	b.sessionsErr = nil
	b.sessions = []tmux.SessionInfo{{Name: "vb/api"}}
	mod, _ = mod.Update(after.fetch()())
	recovered := mod.(Model)
	if len(recovered.rows()) != 1 {
		t.Errorf("rows = %d, want 1 — the pane stopped accepting good polls", len(recovered.rows()))
	}
	if recovered.alerted {
		t.Error("alert not withdrawn once a successful poll showed the bell was gone")
	}
}

// SetWorkspaceAlert is partly-succeed-able: it sets the tab title AND
// writes a BEL to every terminal attached to the workspace, and it
// returns an error if any one of those fails. With several terminals
// open, one that has gone away fails the whole call while the others
// have already been rung.
//
// So the alert edge has to be spent on the attempt. Retrying it means
// re-ringing every healthy terminal — and since the poll repeats every
// two seconds and the bell stays set until the user looks at it, that is
// not one stray beep but a beep every two seconds until they do.
func TestFailedAlertTitleRetriesWithoutRepeatingBell(t *testing.T) {
	b := &fakeBackend{alertErr: errors.New("set title: lost server")}
	b.sessions = []tmux.SessionInfo{{Name: "vb/api", Bell: true}}
	m := NewModel(b, Options{WS: "vb"})

	var mod tea.Model = m
	for i := 0; i < 3; i++ {
		mod, _ = mod.Update(mod.(Model).fetch()())
	}

	if len(b.alerts) != 3 {
		t.Errorf("alerts = %v, want the failed persistent title retried each poll", b.alerts)
	}
	if b.rings != 1 {
		t.Errorf("rings = %d after 3 polls, want exactly 1", b.rings)
	}
	if got := mod.(Model).footer(); !strings.Contains(got, "lost server") {
		t.Errorf("footer = %q, want the title failure surfaced", got)
	}

	b.alertErr = nil
	mod, _ = mod.Update(mod.(Model).fetch()())
	if !mod.(Model).alerted {
		t.Error("successful title retry did not record the persistent alert")
	}
	if b.rings != 1 {
		t.Errorf("successful title retry repeated the one-shot bell: rings=%d", b.rings)
	}
}

func TestFailedAlertRaiseClearsErrorWhenBellDisappears(t *testing.T) {
	b := &fakeBackend{
		sessions: []tmux.SessionInfo{{Name: "vb/api", Bell: true}},
		alertErr: errors.New("set title: lost server"),
	}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(mod.(Model).fetch()())
	if got := mod.(Model).footer(); !strings.Contains(got, "lost server") {
		t.Fatalf("failed raise footer = %q", got)
	}

	b.sessions = []tmux.SessionInfo{{Name: "vb/api"}}
	mod, _ = mod.Update(mod.(Model).fetch()())
	final := mod.(Model)
	if final.alerted {
		t.Error("failed raise was recorded as active")
	}
	if got := final.footer(); got != "" {
		t.Errorf("footer = %q after bell disappeared, want stale raise error cleared", got)
	}
	if len(b.alerts) != 1 {
		t.Errorf("title calls = %v, want no unnecessary clear after raise never landed", b.alerts)
	}
}

func TestFailedOneShotRingIsSurfacedButNotRetried(t *testing.T) {
	b := &fakeBackend{
		sessions: []tmux.SessionInfo{{Name: "vb/api", Bell: true}},
		ringErr:  errors.New("device not configured"),
	}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	for i := 0; i < 3; i++ {
		mod, _ = mod.Update(mod.(Model).fetch()())
	}
	if b.rings != 1 {
		t.Errorf("rings = %d after 3 polls, want exactly 1", b.rings)
	}
	if len(b.alerts) != 1 || !b.alerts[0] {
		t.Errorf("title updates = %v, want successful raise despite ring failure", b.alerts)
	}
	if got := mod.(Model).footer(); !strings.Contains(got, "device not configured") {
		t.Errorf("footer = %q, want the one-shot failure surfaced for the episode", got)
	}
}

// The falling edge is the opposite case and must not be spent. Clearing
// the alert only rewrites the tab title — no terminal gets rung, so a
// retry costs nothing, while giving up pins a 🔔 on the tab for a bell
// the user has already dealt with. That is the failure this whole
// feature exists to prevent, arrived at from the other side.
func TestFailedAlertClearIsRetriedUntilItLands(t *testing.T) {
	b := &fakeBackend{sessions: []tmux.SessionInfo{{Name: "vb/api", Bell: true}}}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(mod.(Model).fetch()())
	if !mod.(Model).alerted {
		t.Fatal("precondition: the bell did not raise the alert")
	}

	// The user looks at the session, so tmux drops the bell — but the
	// call that would clear the title fails.
	b.sessions = []tmux.SessionInfo{{Name: "vb/api"}}
	b.alertErr = errors.New("lost server")
	mod, _ = mod.Update(mod.(Model).fetch()())
	if !mod.(Model).alerted {
		t.Fatal("a failed clear was recorded as done; the tab keeps its 🔔 forever and nothing will try again")
	}
	if got := mod.(Model).footer(); !strings.Contains(got, "lost server") {
		t.Errorf("footer = %q, want the failed clear surfaced", got)
	}

	b.alertErr = nil
	mod, _ = mod.Update(mod.(Model).fetch()())
	final := mod.(Model)
	if final.alerted {
		t.Error("the retry succeeded but the alert is still recorded as raised")
	}
	if len(b.alerts) != 3 || b.alerts[0] != true || b.alerts[1] != false || b.alerts[2] != false {
		t.Errorf("alerts = %v, want [true false false] — raise, failed clear, retried clear", b.alerts)
	}
	if got := final.footer(); got != "" {
		t.Errorf("footer = %q once the clear landed, want empty", got)
	}
}

// A pane that quietly holds the last good rows looks exactly like a pane
// where nothing is happening. Say which it is.
func TestFailedPollSaysTheRowsAreStale(t *testing.T) {
	b := &fakeBackend{sessions: []tmux.SessionInfo{{Name: "vb/api"}}}
	m := NewModel(b, Options{WS: "vb"})
	var mod tea.Model = m
	mod, _ = mod.Update(mod.(Model).fetch()())
	if got := mod.(Model).footer(); got != "" {
		t.Fatalf("precondition: footer = %q on a healthy poll, want empty", got)
	}

	b.sessionsErr = errors.New("no server running")
	mod, _ = mod.Update(mod.(Model).fetch()())
	if got := mod.(Model).footer(); !strings.Contains(got, "no server running") {
		t.Errorf("footer = %q after a failed poll; the rows on screen are frozen and nothing says so", got)
	}

	b.sessionsErr = nil
	mod, _ = mod.Update(mod.(Model).fetch()())
	if got := mod.(Model).footer(); got != "" {
		t.Errorf("footer = %q once polling recovered, want empty — a stale warning that outlives the staleness is just noise", got)
	}
}
