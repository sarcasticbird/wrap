package tree

import (
	"errors"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/gitx"
)

// Run wires the tree TUI with production git functions.
func Run(b Backend, o Options) error {
	if o.Status == nil {
		o.Status = gitx.Status
	}
	if o.Take == nil {
		o.Take = gitx.Take
	}
	if o.Worktrees == nil {
		o.Worktrees = discoverWorktrees
	}
	m := NewModel(b, o)
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	_, err := p.Run()
	return err
}

// EntryNames returns the repository/worktree entry names the tree will render.
// Launch uses the same topology before building the UI so legacy tmux session
// names can be migrated without the migration view drifting from the TUI view.
func EntryNames(root string, repos []gitx.Discovered) ([]string, string) {
	return entryNames(root, repos, discoverWorktrees)
}

// EntryNamesAndPaths returns one topology snapshot for both legacy-name
// migration and path-identity validation. Keeping them in one discovery pass
// prevents migration and validation from authorizing different live views.
func EntryNamesAndPaths(root, ws string, repos []gitx.Discovered) ([]string, map[string]string, string) {
	expanded, rootKids, errText := entryTopology(root, filepath.Base(root), repos, discoverWorktrees)
	names := make([]string, 0, len(rootKids)+len(expanded))
	paths := map[string]string{ws: root}
	for _, d := range append(rootKids, expanded...) {
		names = append(names, d.Name)
		paths[config.SessionName(ws, d.Name)] = d.Path
	}
	return names, paths, errText
}

// discoverWorktrees lists sibling worktrees of a plain repo, excluding
// the repo's own root. Non-repos yield nothing.
func discoverWorktrees(dir string) ([]gitx.Worktree, error) {
	root, err := gitx.Toplevel(dir)
	if err != nil {
		if errors.Is(err, gitx.ErrNotARepo) {
			return nil, nil
		}
		return nil, err
	}
	wts, err := gitx.Worktrees(dir)
	if err != nil {
		return nil, err
	}
	var out []gitx.Worktree
	for _, w := range wts {
		if w.Path != root {
			out = append(out, w)
		}
	}
	return out, nil
}
