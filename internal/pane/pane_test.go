package pane

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// fakeBackend records what the shared keys asked the launcher to do.
type fakeBackend struct {
	killSuccessor  string
	killID         string
	killGeneration string
	killed         []string
	detached       int
	shutdowns      int
	err            error
}

func (f *fakeBackend) KillEntrySession(name, targetID, targetGeneration, successor string) error {
	f.killSuccessor = successor
	f.killID = targetID
	f.killGeneration = targetGeneration
	f.killed = append(f.killed, name)
	return f.err
}
func (f *fakeBackend) DetachUI() error          { f.detached++; return f.err }
func (f *fakeBackend) ShutdownWorkspace() error { f.shutdowns++; return f.err }

func key(s string) tea.KeyMsg {
	if s == "enter" {
		return tea.KeyMsg{Type: tea.KeyEnter}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// An empty row list parks the cursor at 0 rather than leaving a stale
// index. The terms pane empties whenever the last session dies, and the
// two panes used to disagree here — the tree returned the stale cursor.
func TestClampCursorEmptyListParksAtZero(t *testing.T) {
	n := Nav{Cursor: 7}
	if got := n.ClampCursor(0); got != 0 {
		t.Errorf("ClampCursor(0) = %d, want 0", got)
	}
}

func TestClampCursor(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cursor, rows int
		want         int
	}{
		{"in range", 2, 5, 2},
		{"last row", 4, 5, 4},
		{"past end clamps to last", 9, 5, 4},
		{"single row", 3, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := Nav{Cursor: tc.cursor}
			if got := n.ClampCursor(tc.rows); got != tc.want {
				t.Errorf("ClampCursor(%d) with cursor %d = %d, want %d", tc.rows, tc.cursor, got, tc.want)
			}
		})
	}
}

func TestHandleMouseClickSelectsWithoutActivating(t *testing.T) {
	n := Nav{Cursor: 0}
	n.HandleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 3}, 5)
	if n.Cursor != 3 {
		t.Errorf("Cursor = %d, want 3", n.Cursor)
	}
	// A click past the last row is ignored rather than clamped — there is
	// nothing there to select.
	n.HandleMouse(tea.MouseMsg{Action: tea.MouseActionPress, Button: tea.MouseButtonLeft, Y: 99}, 5)
	if n.Cursor != 3 {
		t.Errorf("out-of-range click moved cursor to %d", n.Cursor)
	}
}

func TestHandleMouseWheelStepsOneRowAndStopsAtEnds(t *testing.T) {
	n := Nav{Cursor: 0}
	n.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp}, 3)
	if n.Cursor != 0 {
		t.Errorf("wheel up at top = %d, want 0", n.Cursor)
	}
	n.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown}, 3)
	if n.Cursor != 1 {
		t.Errorf("wheel down = %d, want 1", n.Cursor)
	}
	n.Cursor = 2
	n.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown}, 3)
	if n.Cursor != 2 {
		t.Errorf("wheel down at bottom = %d, want 2", n.Cursor)
	}
}

// x captures the target by name and stable ID; y kills exactly that session even though
// rows resort under the cursor between the two keypresses.
func TestConfirmKillUsesCapturedTarget(t *testing.T) {
	b := &fakeBackend{}
	n := Nav{ConfirmKill: true, ConfirmTarget: "ws/alpha", ConfirmTargetID: "$7", ConfirmTargetGeneration: "generation", Cursor: 0}
	if !n.HandleKey(key("y"), b, 3) {
		t.Fatal("y during confirm should be consumed")
	}
	if len(b.killed) != 1 || b.killed[0] != "ws/alpha" {
		t.Errorf("killed = %v, want [ws/alpha]", b.killed)
	}
	if b.killID != "$7" {
		t.Errorf("stable kill ID = %q, want $7", b.killID)
	}
	if b.killGeneration != "generation" {
		t.Errorf("kill generation = %q, want generation", b.killGeneration)
	}
	if n.ConfirmKill || n.ConfirmTarget != "" {
		t.Errorf("confirm state not cleared: %+v", n)
	}
}

func TestConfirmKillAnyOtherKeyCancels(t *testing.T) {
	b := &fakeBackend{}
	n := Nav{ConfirmKill: true, ConfirmTarget: "ws/alpha"}
	if !n.HandleKey(key("n"), b, 3) {
		t.Fatal("n during confirm should be consumed")
	}
	if len(b.killed) != 0 {
		t.Errorf("cancel still killed %v", b.killed)
	}
	if n.ConfirmKill {
		t.Error("ConfirmKill should be cleared after cancel")
	}
}

func TestConfirmKillSurfacesBackendError(t *testing.T) {
	b := &fakeBackend{err: errors.New("boom")}
	n := Nav{ConfirmKill: true, ConfirmTarget: "ws/alpha"}
	n.HandleKey(key("y"), b, 3)
	if !strings.Contains(n.ErrText, "boom") {
		t.Errorf("ErrText = %q, want the backend error", n.ErrText)
	}
}

func TestShutdownConfirmFlow(t *testing.T) {
	b := &fakeBackend{}
	n := Nav{}
	if !n.HandleKey(key("Q"), b, 3) {
		t.Fatal("Q should be consumed")
	}
	if !n.ConfirmShutdown {
		t.Fatal("Q should arm the shutdown confirmation, not shut down")
	}
	if b.shutdowns != 0 {
		t.Fatal("Q shut down without confirmation")
	}
	n.HandleKey(key("y"), b, 3)
	if b.shutdowns != 1 {
		t.Errorf("shutdowns = %d, want 1", b.shutdowns)
	}
}

func TestDetachAndCursorKeys(t *testing.T) {
	b := &fakeBackend{}
	n := Nav{Cursor: 0}
	if !n.HandleKey(key("q"), b, 3) || b.detached != 1 {
		t.Errorf("q should detach: detached=%d", b.detached)
	}
	n.HandleKey(key("j"), b, 3)
	if n.Cursor != 1 {
		t.Errorf("j: Cursor = %d, want 1", n.Cursor)
	}
	n.HandleKey(key("k"), b, 3)
	if n.Cursor != 0 {
		t.Errorf("k: Cursor = %d, want 0", n.Cursor)
	}
	n.HandleKey(key("k"), b, 3)
	if n.Cursor != 0 {
		t.Errorf("k at top: Cursor = %d, want 0", n.Cursor)
	}
	n.Cursor = 2
	n.HandleKey(key("j"), b, 3)
	if n.Cursor != 2 {
		t.Errorf("j at bottom: Cursor = %d, want 2", n.Cursor)
	}
}

// Keys the shared handler does not own are left for the pane's own switch.
func TestUnhandledKeyIsNotConsumed(t *testing.T) {
	b := &fakeBackend{}
	n := Nav{}
	for _, k := range []string{"enter", "n", "r", "x", "l", "h"} {
		if n.HandleKey(key(k), b, 3) {
			t.Errorf("%q should not be consumed by the shared handler", k)
		}
	}
}

// The cursor and the terminal on screen are independent, so x routinely
// targets a session the user is not looking at. The prompt has to say so.
func TestConfirmFooterNamesTheShowingSessionWhenItDiffers(t *testing.T) {
	n := Nav{ConfirmKill: true, ConfirmTarget: "ws/api"}
	got := n.ConfirmFooter("ws/main")
	if !strings.Contains(got, "kill ws/api?") {
		t.Errorf("footer should name the target: %q", got)
	}
	if !strings.Contains(got, "showing ws/main") {
		t.Errorf("footer should name what's on screen when it differs: %q", got)
	}
}

// When the cursor IS the session on screen there is nothing to disambiguate.
func TestConfirmFooterOmitsShowingWhenItIsTheTarget(t *testing.T) {
	n := Nav{ConfirmKill: true, ConfirmTarget: "ws/api"}
	if got := n.ConfirmFooter("ws/api"); strings.Contains(got, "showing") {
		t.Errorf("footer should not add a parenthetical for the same session: %q", got)
	}
}

func TestArmKillCapturesTargetAndSuccessor(t *testing.T) {
	b := &fakeBackend{}
	var n Nav
	n.ArmKill("ws/api", "$7", "generation", "ws/main")
	if !n.ConfirmKill || n.ConfirmTarget != "ws/api" || n.ConfirmTargetID != "$7" || n.ConfirmTargetGeneration != "generation" || n.ConfirmSuccessor != "ws/main" {
		t.Fatalf("ArmKill state = %+v", n)
	}
	n.HandleKey(key("y"), b, 3)
	if len(b.killed) != 1 || b.killed[0] != "ws/api" || b.killID != "$7" || b.killSuccessor != "ws/main" {
		t.Errorf("killed=%v id=%q successor=%q, want [ws/api] / $7 / ws/main", b.killed, b.killID, b.killSuccessor)
	}
	if n.ConfirmSuccessor != "" {
		t.Errorf("successor not cleared after the confirmation resolved: %q", n.ConfirmSuccessor)
	}
}

func TestIsKillTarget(t *testing.T) {
	n := Nav{ConfirmKill: true, ConfirmTarget: "ws/api"}
	if !n.IsKillTarget("ws/api") {
		t.Error("the armed target should be reported as the kill target")
	}
	if n.IsKillTarget("ws/main") {
		t.Error("a different session must not be reported as the kill target")
	}
	// File rows and other session-less rows must never match.
	if n.IsKillTarget("") {
		t.Error("an empty session must never be the kill target")
	}
	// With no confirmation pending nothing is a target.
	if (Nav{}).IsKillTarget("ws/api") {
		t.Error("nothing is a kill target without a pending confirmation")
	}
}

func TestConfirmFooterPrecedence(t *testing.T) {
	if got := (Nav{}).ConfirmFooter(""); got != "" {
		t.Errorf("idle footer = %q, want empty", got)
	}
	if got := (Nav{ConfirmKill: true, ConfirmTarget: "ws/a"}).ConfirmFooter(""); !strings.Contains(got, "kill ws/a?") {
		t.Errorf("kill footer = %q", got)
	}
	if got := (Nav{ConfirmShutdown: true}).ConfirmFooter(""); !strings.Contains(got, "shutdown workspace?") {
		t.Errorf("shutdown footer = %q", got)
	}
	if got := (Nav{ErrText: "bad"}).ConfirmFooter(""); !strings.Contains(got, "bad") {
		t.Errorf("error footer = %q", got)
	}
	// A pending confirmation outranks a stale error.
	n := Nav{ConfirmKill: true, ConfirmTarget: "ws/a", ErrText: "old"}
	if got := n.ConfirmFooter(""); strings.Contains(got, "old") {
		t.Errorf("confirm footer should outrank the error: %q", got)
	}
}
