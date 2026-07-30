package workspacesui

import (
	"errors"
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/sarcasticbird/wrap/internal/workspaces"
)

func applySnapshot(t *testing.T, m Model, snapshot workspaces.Snapshot, err error) Model {
	t.Helper()
	mod, _ := m.Update(snapshotMsg{snapshot: snapshot, err: err})
	got, ok := mod.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want Model", mod)
	}
	return got
}

func TestSuccessfulRefreshSortsAndPreservesWorkspaceIdentity(t *testing.T) {
	m := NewModel(Options{})
	m = applySnapshot(t, m, workspaces.Snapshot{Workspaces: []workspaces.Workspace{
		{Name: "beta", Root: "/work/beta"},
		{Name: "alpha", Root: "/work/alpha"},
	}}, nil)
	m.cursor = 1

	m = applySnapshot(t, m, workspaces.Snapshot{Workspaces: []workspaces.Workspace{
		{Name: "beta", Root: "/work/beta"},
		{Name: "aardvark", Root: "/work/aardvark"},
		{Name: "alpha", Root: "/work/alpha"},
	}}, nil)

	if got := m.rows[m.cursor].Name; got != "beta" {
		t.Fatalf("selected workspace after refresh = %q, want beta", got)
	}
}

func TestFailedRefreshRetainsRowsAndMarksThemStale(t *testing.T) {
	m := NewModel(Options{})
	m = applySnapshot(t, m, workspaces.Snapshot{Workspaces: []workspaces.Workspace{{
		Name: "alpha", Root: "/work/alpha",
	}}}, nil)

	m = applySnapshot(t, m, workspaces.Snapshot{}, errors.New("tmux unavailable"))

	if len(m.rows) != 1 || m.rows[0].Name != "alpha" {
		t.Fatalf("rows after failed refresh = %+v", m.rows)
	}
	if !strings.Contains(m.stale, "tmux unavailable") {
		t.Fatalf("stale = %q", m.stale)
	}
}

func TestLaterSuccessfulRefreshClearsStaleState(t *testing.T) {
	m := NewModel(Options{})
	m = applySnapshot(t, m, workspaces.Snapshot{}, errors.New("tmux unavailable"))
	m = applySnapshot(t, m, workspaces.Snapshot{Workspaces: []workspaces.Workspace{{
		Name: "alpha", Root: "/work/alpha",
	}}}, nil)

	if m.stale != "" {
		t.Fatalf("stale = %q, want cleared", m.stale)
	}
}

func TestEnterSelectsRootAndQQuitsWithoutSelection(t *testing.T) {
	t.Run("enter", func(t *testing.T) {
		m := NewModel(Options{})
		m = applySnapshot(t, m, workspaces.Snapshot{Workspaces: []workspaces.Workspace{{
			Name: "alpha", Root: "/work/alpha",
		}}}, nil)

		mod, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
		got := mod.(Model)
		if got.selected.Root != "/work/alpha" {
			t.Fatalf("selected root = %q", got.selected.Root)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("enter command returned %T, want tea.QuitMsg", cmd())
		}
	})

	t.Run("q", func(t *testing.T) {
		m := NewModel(Options{})
		m = applySnapshot(t, m, workspaces.Snapshot{Workspaces: []workspaces.Workspace{{
			Name: "alpha", Root: "/work/alpha",
		}}}, nil)

		mod, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
		got := mod.(Model)
		if got.selected != (workspaces.Workspace{}) {
			t.Fatalf("selected workspace = %+v, want empty", got.selected)
		}
		if _, ok := cmd().(tea.QuitMsg); !ok {
			t.Fatalf("q command returned %T, want tea.QuitMsg", cmd())
		}
	})
}

func TestEnterPreservesWorkspaceIdentity(t *testing.T) {
	want := workspaces.Workspace{
		Name: "alias", Root: "/real/service", Recover: true,
	}
	m := applySnapshot(t, NewModel(Options{}), workspaces.Snapshot{
		Workspaces: []workspaces.Workspace{want},
	}, nil)

	mod, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := mod.(Model)
	if got.selected != want {
		t.Fatalf("selected workspace = %+v, want %+v", got.selected, want)
	}
}

func TestEnterDoesNotSelectStaleWorkspace(t *testing.T) {
	m := applySnapshot(t, NewModel(Options{}), workspaces.Snapshot{
		Workspaces: []workspaces.Workspace{{
			Name: "alpha", Root: "/work/alpha",
		}},
	}, nil)
	m = applySnapshot(t, m, workspaces.Snapshot{}, errors.New("tmux unavailable"))

	mod, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := mod.(Model)
	if got.selected != (workspaces.Workspace{}) {
		t.Fatalf("selected workspace = %+v, want no stale selection", got.selected)
	}
	if cmd != nil {
		t.Fatal("Enter on stale rows quit the selector")
	}
}

func TestEnterDoesNotSelectInvisibleWorkspace(t *testing.T) {
	m := applySnapshot(t, NewModel(Options{}), workspaces.Snapshot{
		Workspaces: []workspaces.Workspace{{
			Name: "alpha", Root: "/work/alpha",
		}},
	}, nil)
	m.height = 4

	mod, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := mod.(Model)
	if got.selected != (workspaces.Workspace{}) {
		t.Fatalf("selected workspace = %+v, want no invisible selection", got.selected)
	}
	if cmd != nil {
		t.Fatal("Enter on an invisible row quit the selector")
	}
}

func TestKeyboardAndMouseMoveCursor(t *testing.T) {
	snapshot := workspaces.Snapshot{Workspaces: []workspaces.Workspace{
		{Name: "alpha", Root: "/work/alpha"},
		{Name: "beta", Root: "/work/beta"},
		{Name: "gamma", Root: "/work/gamma"},
	}}
	m := applySnapshot(t, NewModel(Options{}), snapshot, nil)

	mod, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = mod.(Model)
	if got := m.rows[m.cursor].Name; got != "beta" {
		t.Fatalf("down selected %q, want beta", got)
	}
	mod, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = mod.(Model)
	if got := m.rows[m.cursor].Name; got != "alpha" {
		t.Fatalf("up selected %q, want alpha", got)
	}

	// Heading is row 0, spacer row 1, then each workspace owns two lines.
	mod, _ = m.Update(tea.MouseMsg{
		Y: 4, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	})
	m = mod.(Model)
	if got := m.rows[m.cursor].Name; got != "beta" {
		t.Fatalf("click on beta name selected %q", got)
	}
	mod, _ = m.Update(tea.MouseMsg{
		Y: 7, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft,
	})
	m = mod.(Model)
	if got := m.rows[m.cursor].Name; got != "gamma" {
		t.Fatalf("click on gamma path selected %q", got)
	}
	mod, _ = m.Update(tea.MouseMsg{
		Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp,
	})
	m = mod.(Model)
	if got := m.rows[m.cursor].Name; got != "beta" {
		t.Fatalf("wheel up selected %q", got)
	}
}

func TestViewRendersNameFullRootStatusesAndFooter(t *testing.T) {
	m := NewModel(Options{})
	m.width = 80
	m = applySnapshot(t, m, workspaces.Snapshot{Workspaces: []workspaces.Workspace{
		{Name: "beta", Root: "/work/beta", Attached: true},
		{Name: "alpha", Root: "/work/alpha", Recover: true},
	}}, nil)

	view := ansi.Strip(m.View())
	for _, want := range []string{
		"Active Wraps",
		"▸ alpha  recover",
		"  /work/alpha",
		"  beta  attached",
		"  /work/beta",
		"enter attach · q quit",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("View missing %q:\n%s", want, view)
		}
	}
}

func TestViewTruncatesLongRootsFromTheLeftByCellWidth(t *testing.T) {
	m := NewModel(Options{})
	m.width = 20
	m = applySnapshot(t, m, workspaces.Snapshot{Workspaces: []workspaces.Workspace{{
		Name: "alpha", Root: "/Users/alex/Projects/very/deep/project",
	}}}, nil)

	lines := strings.Split(ansi.Strip(m.View()), "\n")
	if len(lines) < 4 {
		t.Fatalf("View lines = %v", lines)
	}
	root := lines[3]
	if !strings.HasPrefix(root, "  …") || !strings.HasSuffix(root, "deep/project") {
		t.Fatalf("truncated root = %q", root)
	}
	for _, line := range lines {
		if got := runewidth.StringWidth(line); got > m.width {
			t.Fatalf("line width = %d, want <= %d: %q", got, m.width, line)
		}
	}
}

func TestViewShowsEmptyStateWarningsStaleAndInitialNote(t *testing.T) {
	t.Run("empty and initial note", func(t *testing.T) {
		m := NewModel(Options{InitialNote: "launch failed"})
		view := ansi.Strip(m.View())
		for _, want := range []string{"No active wraps", "launch failed", "enter attach · q quit"} {
			if !strings.Contains(view, want) {
				t.Errorf("View missing %q:\n%s", want, view)
			}
		}
	})

	t.Run("warning", func(t *testing.T) {
		m := applySnapshot(t, NewModel(Options{}), workspaces.Snapshot{
			Warnings: []string{"workspace metadata unavailable"},
		}, nil)
		if view := ansi.Strip(m.View()); !strings.Contains(view, "workspace metadata unavailable") {
			t.Fatalf("View missing warning:\n%s", view)
		}
	})

	t.Run("launch failure outranks warning", func(t *testing.T) {
		m := applySnapshot(t, NewModel(Options{InitialNote: "launch failed"}), workspaces.Snapshot{
			Warnings: []string{"unrelated metadata warning"},
		}, nil)
		view := ansi.Strip(m.View())
		if !strings.Contains(view, "launch failed") {
			t.Fatalf("View hid launch failure behind warning:\n%s", view)
		}
	})

	t.Run("stale outranks note", func(t *testing.T) {
		m := NewModel(Options{InitialNote: "old launch failure"})
		m = applySnapshot(t, m, workspaces.Snapshot{}, errors.New("tmux down"))
		view := ansi.Strip(m.View())
		if !strings.Contains(view, "rows stale: tmux down") {
			t.Fatalf("View missing stale error:\n%s", view)
		}
		if strings.Contains(view, "old launch failure") {
			t.Fatalf("View rendered lower-priority note with stale error:\n%s", view)
		}
	})
}

func TestViewEscapesControlCharactersInRootsAndWarnings(t *testing.T) {
	m := applySnapshot(t, NewModel(Options{}), workspaces.Snapshot{
		Workspaces: []workspaces.Workspace{{
			Name: "alpha", Root: "/work/line\nbreak",
		}},
		Warnings: []string{"bad\tmetadata"},
	}, nil)
	view := ansi.Strip(m.View())
	if strings.Contains(view, "/work/line\nbreak") || strings.Contains(view, "bad\tmetadata") {
		t.Fatalf("View rendered raw control characters:\n%s", view)
	}
	for _, want := range []string{`/work/line\nbreak`, `bad\tmetadata`} {
		if !strings.Contains(view, want) {
			t.Errorf("View missing escaped %q:\n%s", want, view)
		}
	}
}

func TestRunReturnsNoSelectionWhenUserQuits(t *testing.T) {
	selected, ok, err := Run(Options{
		programOptions: []tea.ProgramOption{
			tea.WithInput(strings.NewReader("q")),
			tea.WithOutput(io.Discard),
			tea.WithoutRenderer(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok || selected != (workspaces.Workspace{}) {
		t.Fatalf("selection = %+v, ok=%v; want none", selected, ok)
	}
}
