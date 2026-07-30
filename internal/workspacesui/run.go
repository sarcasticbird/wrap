package workspacesui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sarcasticbird/wrap/internal/workspaces"
)

// Run starts the standalone selector and returns the selected workspace
// identity. ok is false when the user quits without selecting a workspace.
func Run(options Options) (workspaces.Workspace, bool, error) {
	programOptions := []tea.ProgramOption{
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	}
	programOptions = append(programOptions, options.programOptions...)
	final, err := tea.NewProgram(NewModel(options), programOptions...).Run()
	if err != nil {
		return workspaces.Workspace{}, false, err
	}
	model, ok := final.(Model)
	if !ok {
		return workspaces.Workspace{}, false,
			fmt.Errorf("workspace selector returned %T, want workspacesui.Model", final)
	}
	return model.selected, model.selected.Name != "", nil
}
