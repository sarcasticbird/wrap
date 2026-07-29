// Package workspace decides what `wrap [target]` means and discovers the
// repos it should show: "" resolves to the current directory, any other
// target must be an existing directory. There is no other kind of
// workspace — every wrap invocation targets a folder.
package workspace

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/gitx"
	"github.com/sarcasticbird/wrap/internal/state"
)

// Workspace is a folder wrap has been pointed at: its discovered repos,
// the session command to run in them, and the pane-focus key bindings.
type Workspace struct {
	Name  string
	Root  string
	Repos []gitx.Discovered
	Cmd   string
	Keys  config.Keys
	// TreeSide is which column the tree/terms pair renders in ("left" or
	// "right"), carried from config (default "left" when cfg is nil).
	TreeSide string
	// TreeWidth is the tree/terms column's width as a percent of the
	// window, carried from config (default 25 when cfg is nil).
	TreeWidth int
	// WalkDepth is how many directory levels discovery descended,
	// carried from config (default 1 when cfg is nil). It doesn't affect
	// discovery here (discover already ran with it) — it's carried so
	// callers can detect a config change and rebuild the chrome, since
	// pane subprocesses re-derive discovery at spawn and bake this in.
	WalkDepth int
}

func (w *Workspace) Meta() state.Meta {
	return state.Meta{Kind: "folder", Root: w.Root}
}

// Resolve maps `wrap [target]` to a workspace: "" is the current
// directory; any other target must be an existing directory. An empty
// (no-repos) folder is a valid workspace — only discovery failures and
// bad targets error.
func Resolve(target, cwd string, cfg *config.Config,
	discover func(string) ([]gitx.Discovered, []string, error),
) (*Workspace, []string, error) {
	dir := cwd
	if target != "" {
		dir = target
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("workspace %s: %w", dir, err)
	}
	root, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("workspace %s: %w", abs, err)
	}
	fi, err := os.Stat(root)
	if err != nil {
		return nil, nil, fmt.Errorf("workspace %s: %w", root, err)
	}
	if !fi.IsDir() {
		return nil, nil, fmt.Errorf("workspace %s: not a directory", root)
	}
	dir = root
	found, warns, err := discover(dir)
	if err != nil {
		return nil, warns, err
	}
	w := folderWS(dir, found, cfg)
	// Keep the identity derived from the path the user opened. Before roots
	// were canonicalized, a symlink named "api" produced the "api"
	// workspace; changing that name would strand its sessions and state.
	base := filepath.Base(abs)
	if base == string(filepath.Separator) || base == "." {
		base = "root"
	}
	w.Name = config.SanitizeName(base)
	if err := config.ValidateWorkspaceName(w.Name); err != nil {
		return nil, warns, fmt.Errorf("resolve workspace %q: %w", w.Name, err)
	}
	return w, warns, nil
}

// FromMeta reconstructs the workspace a pane subprocess belongs to. name
// is the ws argument the subprocess was invoked with (already the
// sanitized identity Resolve produced); m.Root is where to rediscover
// repos from.
func FromMeta(name string, m state.Meta, cfg *config.Config,
	discover func(string) ([]gitx.Discovered, []string, error),
) (*Workspace, []string, error) {
	if err := config.ValidateWorkspaceName(name); err != nil {
		return nil, nil, fmt.Errorf("reconstruct workspace %q: %w", name, err)
	}
	found, warns, err := discover(m.Root)
	if err != nil {
		return nil, warns, err
	}
	w := folderWS(m.Root, found, cfg)
	w.Name = name
	return w, warns, nil
}

// InitialSelection is what a workspace opens with before the user picks
// anything: a terminal at the workspace root. The second return is the
// command that session should run (empty = login shell).
func (w *Workspace) InitialSelection() (state.Selection, string) {
	return state.Selection{
		Entry:   w.Name,
		Session: w.Name,
		Path:    w.Root,
	}, w.Cmd
}

func folderWS(dir string, found []gitx.Discovered, cfg *config.Config) *Workspace {
	keys := config.Keys{}
	cmd := ""
	treeSide := "left"
	treeWidth := 25
	walkDepth := 1
	if cfg != nil {
		keys = cfg.Keys
		cmd = cfg.Defaults.Cmd
		if cfg.TreeSide != "" {
			treeSide = cfg.TreeSide
		}
		if cfg.TreeWidth != 0 {
			treeWidth = cfg.TreeWidth
		}
		if cfg.WalkDepth != 0 {
			walkDepth = cfg.WalkDepth
		}
	}
	return &Workspace{
		Name:      config.SanitizeName(filepath.Base(dir)),
		Root:      dir,
		Repos:     found,
		Cmd:       cmd,
		Keys:      keys,
		TreeSide:  treeSide,
		TreeWidth: treeWidth,
		WalkDepth: walkDepth,
	}
}
