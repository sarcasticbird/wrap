// Package pane holds what the tree and terms panes have in common: the
// cursor, the two-step confirmation behind x and Q, the mouse rules, and
// the styles both render with. Both models embed Nav.
//
// These were duplicated byte-for-byte between the two packages until the
// copies drifted — clampCursor disagreed on what to do with an empty row
// list — so the shared behavior lives here and each pane keeps only what
// is genuinely its own.
package pane

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Styles both panes render with.
var (
	CursorStyle = lipgloss.NewStyle().Reverse(true)
	DimStyle    = lipgloss.NewStyle().Faint(true)
	AlertStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
)

// Backend is what Nav needs from the launcher to service the shared keys.
// Each pane's own Backend interface is a superset of this one.
type Backend interface {
	KillEntrySession(name, targetID, targetGeneration, successor string) error
	DetachUI() error
	ShutdownWorkspace() error
}

// Nav is the cursor and confirmation state both panes carry.
type Nav struct {
	Cursor      int
	ConfirmKill bool
	// ConfirmTarget is the session captured when x was pressed. Rows
	// resort under the cursor on every 2s tick, so the target must be
	// captured by name at x-time rather than re-read at y-time.
	ConfirmTarget string
	// ConfirmTargetID is tmux's stable ID for ConfirmTarget at x-time.
	// A replacement session can reuse the name before y is pressed; the
	// captured ID ensures confirmation can only kill the original.
	ConfirmTargetID string
	// ConfirmTargetGeneration identifies the work-session tmux server that
	// issued ConfirmTargetID. tmux can reuse IDs after a server restart.
	ConfirmTargetGeneration string
	// ConfirmSuccessor is where the terminal pane should land if the
	// killed session was the one on screen. Captured from the same row
	// snapshot as ConfirmTarget, for the same reason.
	ConfirmSuccessor string
	ConfirmShutdown  bool
	ErrText          string
	Width, Height    int
}

// ArmKill starts the kill confirmation for target, landing on successor
// afterwards if target turns out to be the session on screen.
func (n *Nav) ArmKill(target, targetID, targetGeneration, successor string) {
	n.ConfirmKill = true
	n.ConfirmTarget = target
	n.ConfirmTargetID = targetID
	n.ConfirmTargetGeneration = targetGeneration
	n.ConfirmSuccessor = successor
}

// IsKillTarget reports whether session is the one a pending confirmation
// would kill, so the pane can render that row in AlertStyle — the cursor
// and the session on screen are often different rows, and the row about to
// die should be the one the eye lands on.
func (n Nav) IsKillTarget(session string) bool {
	return n.ConfirmKill && session != "" && session == n.ConfirmTarget
}

// ClampCursor returns the cursor bounded to a list of rowCount rows.
//
// An empty list parks the cursor at 0. The tree always renders at least
// its root row so it never sees that case; the terms pane empties out
// whenever the last session dies, and leaving a stale index there would
// point past the first row to reappear.
func (n Nav) ClampCursor(rowCount int) int {
	if rowCount == 0 {
		return 0
	}
	if n.Cursor >= rowCount {
		return rowCount - 1
	}
	return n.Cursor
}

// VisibleRange returns a half-open row window of at most capacity entries that
// contains anchor. Callers choose the anchor: normally the cursor, or the
// pending kill target while confirmation is active.
func VisibleRange(rowCount, capacity, anchor int) (int, int) {
	if rowCount <= 0 {
		return 0, 0
	}
	if capacity <= 0 || capacity > rowCount {
		capacity = rowCount
	}
	if anchor < 0 {
		anchor = 0
	}
	if anchor >= rowCount {
		anchor = rowCount - 1
	}
	start := 0
	if anchor >= capacity {
		start = anchor - capacity + 1
	}
	if start+capacity > rowCount {
		start = rowCount - capacity
	}
	return start, start + capacity
}

// HandleMouse applies the shared mouse rules: a left click only moves the
// cursor (Enter activates — a click that switched terminals made every
// click-to-focus steal the middle pane), and the wheel steps one row.
//
// Wheel events report Action as the zero value, MouseActionPress, in
// bubbletea v1.3.10, so dispatch is on Button alone.
func (n *Nav) HandleMouse(msg tea.MouseMsg, rowCount int) {
	switch {
	case msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft:
		// Pane models translate physical chrome rows before calling this helper.
		idx := msg.Y
		if idx >= 0 && idx < rowCount {
			n.Cursor = idx
		}
	case msg.Button == tea.MouseButtonWheelUp:
		if n.Cursor > 0 {
			n.Cursor--
		}
	case msg.Button == tea.MouseButtonWheelDown:
		if n.Cursor < rowCount-1 {
			n.Cursor++
		}
	}
}

// HandleKey services the keys both panes share — the pending y/n
// confirmations first, then Q, q and cursor movement — and reports
// whether it consumed the key. A false return leaves the key for the
// pane's own switch.
//
// Both confirmations clear on ANY key: y acts, anything else cancels.
func (n *Nav) HandleKey(msg tea.KeyMsg, b Backend, rowCount int) bool {
	if n.ConfirmKill {
		n.ConfirmKill = false
		target, targetID, generation, successor := n.ConfirmTarget, n.ConfirmTargetID, n.ConfirmTargetGeneration, n.ConfirmSuccessor
		n.ConfirmTarget, n.ConfirmTargetID, n.ConfirmTargetGeneration, n.ConfirmSuccessor = "", "", "", ""
		if msg.String() == "y" {
			if err := b.KillEntrySession(target, targetID, generation, successor); err != nil {
				n.ErrText = err.Error()
			}
		}
		return true
	}
	if n.ConfirmShutdown {
		n.ConfirmShutdown = false
		if msg.String() == "y" {
			if err := b.ShutdownWorkspace(); err != nil {
				n.ErrText = err.Error()
			}
		}
		return true
	}
	switch msg.String() {
	case "Q":
		n.ConfirmShutdown = true
		return true
	case "q":
		if err := b.DetachUI(); err != nil {
			n.ErrText = err.Error()
		}
		return true
	case "j", "down":
		if n.Cursor < rowCount-1 {
			n.Cursor++
		}
		return true
	case "k", "up":
		if n.Cursor > 0 {
			n.Cursor--
		}
		return true
	}
	return false
}

// ConfirmFooter renders the pending confirmation or error line both panes
// show. Empty when there is nothing to report; each pane appends its own
// footer content after this.
//
// showing is the session currently on screen. When it differs from the
// kill target the prompt says so: the cursor and the terminal on screen
// are independent, so x routinely targets a session the user is not
// looking at, and a bare "kill X?" gives them no way to notice.
func (n Nav) ConfirmFooter(showing string) string {
	if n.ConfirmKill {
		s := " kill " + n.ConfirmTarget + "?"
		if showing != "" && showing != n.ConfirmTarget {
			s += " (showing " + showing + ")"
		}
		return AlertStyle.Render(s + " y/n ")
	}
	if n.ConfirmShutdown {
		return AlertStyle.Render(" shutdown workspace? y/n ")
	}
	if n.ErrText != "" {
		return AlertStyle.Render(" " + SafeLabel(n.ErrText) + " ")
	}
	return ""
}
