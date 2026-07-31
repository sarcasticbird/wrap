package terms

import (
	"errors"

	tea "github.com/charmbracelet/bubbletea"
)

// Run wires the terms TUI.
//
// WithMouseCellMotion is required for bubbletea to deliver MouseMsg at
// all: without it the pane's whole mouse path is unreachable and a click
// only moves tmux's pane focus, even though the model handles clicks and
// the wheel exactly as the tree pane does.
func Run(b Backend, o Options) error {
	m := NewModel(b, o)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	return runWithMirrorPaneCleanup(b, func() error {
		_, err := p.Run()
		return err
	})
}

func runWithMirrorPaneCleanup(b Backend, run func() error) (err error) {
	defer func() {
		err = errors.Join(err, b.RestoreMirrorPaneHeight())
	}()
	return run()
}
