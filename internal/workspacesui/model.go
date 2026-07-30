// Package workspacesui renders the standalone active-workspace selector.
package workspacesui

import (
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/sarcasticbird/wrap/internal/pane"
	"github.com/sarcasticbird/wrap/internal/workspaces"
)

type DiscoverFunc func() (workspaces.Snapshot, error)

type Options struct {
	Discover    DiscoverFunc
	InitialNote string

	programOptions []tea.ProgramOption
}

type tickMsg struct{}

type snapshotMsg struct {
	snapshot workspaces.Snapshot
	err      error
}

type Model struct {
	discover     DiscoverFunc
	rows         []workspaces.Workspace
	warnings     []string
	cursor       int
	selected     workspaces.Workspace
	stale        string
	initialNote  string
	width        int
	height       int
	polling      bool
	timerPending bool
}

func NewModel(options Options) Model {
	return Model{
		discover:    options.Discover,
		initialNote: options.InitialNote,
	}
}

func (m Model) Init() tea.Cmd {
	return func() tea.Msg { return tickMsg{} }
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		m.timerPending = false
		if m.polling {
			return m, nil
		}
		m.polling = true
		return m, m.fetch()
	case snapshotMsg:
		m.polling = false
		if msg.err != nil {
			m.stale = msg.err.Error()
			return m.scheduleTick()
		}
		selected := ""
		if m.cursor >= 0 && m.cursor < len(m.rows) {
			selected = m.rows[m.cursor].Name
		}
		m.rows = append([]workspaces.Workspace(nil), msg.snapshot.Workspaces...)
		sort.Slice(m.rows, func(i, j int) bool { return m.rows[i].Name < m.rows[j].Name })
		m.warnings = append([]string(nil), msg.snapshot.Warnings...)
		m.stale = ""
		if selected != "" {
			for i := range m.rows {
				if m.rows[i].Name == selected {
					m.cursor = i
					return m.scheduleTick()
				}
			}
		}
		m.clampCursor()
		return m.scheduleTick()
	case tea.KeyMsg:
		switch msg.String() {
		case "q":
			m.selected = workspaces.Workspace{}
			return m, tea.Quit
		case "enter":
			start, end := m.viewport()
			if m.stale == "" && m.cursor >= start && m.cursor < end {
				m.selected = m.rows[m.cursor]
				return m, tea.Quit
			}
		case "j", "down":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		case "k", "up":
			if m.cursor > 0 {
				m.cursor--
			}
		}
		return m, nil
	case tea.MouseMsg:
		switch msg.Button {
		case tea.MouseButtonWheelUp:
			if m.cursor > 0 {
				m.cursor--
			}
		case tea.MouseButtonWheelDown:
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		default:
			if msg.Action == tea.MouseActionPress && msg.Button == tea.MouseButtonLeft && msg.Y >= 2 {
				start, end := m.viewport()
				physical := msg.Y - 2
				index := start + physical/2
				if physical >= 0 && physical < (end-start)*2 &&
					index >= 0 && index < len(m.rows) {
					m.cursor = index
				}
			}
		}
		return m, nil
	}
	return m, nil
}

func (m Model) fetch() tea.Cmd {
	return func() tea.Msg {
		if m.discover == nil {
			return snapshotMsg{}
		}
		snapshot, err := m.discover()
		return snapshotMsg{snapshot: snapshot, err: err}
	}
}

func (m Model) scheduleTick() (Model, tea.Cmd) {
	if m.timerPending {
		return m, nil
	}
	m.timerPending = true
	return m, nextTick()
}

func nextTick() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *Model) clampCursor() {
	switch {
	case len(m.rows) == 0:
		m.cursor = 0
	case m.cursor >= len(m.rows):
		m.cursor = len(m.rows) - 1
	case m.cursor < 0:
		m.cursor = 0
	}
}

var headingStyle = lipgloss.NewStyle().Bold(true)

func (m Model) View() string {
	lines := []string{
		headingStyle.Render(truncateRight("Active Wraps", m.width)),
		"",
	}
	if len(m.rows) == 0 {
		lines = append(lines, pane.DimStyle.Render(truncateRight("  No active wraps", m.width)))
	} else {
		start, end := m.viewport()
		for i := start; i < end; i++ {
			workspace := m.rows[i]
			prefix := "  "
			if i == m.cursor {
				prefix = "▸ "
			}
			line := prefix + pane.SafeLabel(workspace.Name)
			switch {
			case workspace.Recover:
				line += "  recover"
			case workspace.Attached:
				line += "  attached"
			}
			line = truncateRight(line, m.width)
			if i == m.cursor {
				line = pane.CursorStyle.Render(line)
			}
			lines = append(lines, line, pane.DimStyle.Render(m.renderRoot(workspace.Root)))
		}
	}

	status := m.statusLine()
	reserved := 1 // action footer
	if status != "" {
		reserved++
	}
	if m.height > 0 {
		for len(lines)+reserved < m.height {
			lines = append(lines, "")
		}
	}
	if status != "" {
		lines = append(lines, pane.AlertStyle.Render(truncateRight(status, m.width)))
	}
	lines = append(lines, pane.DimStyle.Render(truncateRight("enter attach · q quit", m.width)))
	return strings.Join(lines, "\n")
}

func (m Model) viewport() (int, int) {
	if len(m.rows) == 0 {
		return 0, 0
	}
	capacity := len(m.rows)
	if m.height > 0 {
		reserved := 3 // heading, spacer, action footer
		if m.statusLine() != "" {
			reserved++
		}
		capacity = (m.height - reserved) / 2
		if capacity < 0 {
			capacity = 0
		}
		if capacity > len(m.rows) {
			capacity = len(m.rows)
		}
	}
	if capacity == 0 {
		return 0, 0
	}
	start := 0
	if m.cursor >= capacity {
		start = m.cursor - capacity + 1
	}
	if start+capacity > len(m.rows) {
		start = len(m.rows) - capacity
	}
	return start, start + capacity
}

func (m Model) renderRoot(root string) string {
	const prefix = "  "
	value := pane.SafeLabel(root)
	if m.width > 0 {
		value = truncateLeft(value, m.width-runewidth.StringWidth(prefix))
	}
	return truncateRight(prefix+value, m.width)
}

func (m Model) statusLine() string {
	switch {
	case m.stale != "":
		return "rows stale: " + pane.SafeLabel(m.stale)
	case m.initialNote != "":
		return pane.SafeLabel(m.initialNote)
	case len(m.warnings) > 0:
		return pane.SafeLabel(strings.Join(m.warnings, "; "))
	default:
		return ""
	}
}

func truncateRight(value string, width int) string {
	if width <= 0 {
		return value
	}
	return runewidth.Truncate(value, width, "")
}

func truncateLeft(value string, width int) string {
	if width <= 0 {
		return ""
	}
	if runewidth.StringWidth(value) <= width {
		return value
	}
	cut := runewidth.StringWidth(value) - width + runewidth.StringWidth("…")
	return runewidth.TruncateLeft(value, cut, "…")
}
