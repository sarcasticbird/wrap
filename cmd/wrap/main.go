package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/gitx"
	"github.com/sarcasticbird/wrap/internal/launcher"
	"github.com/sarcasticbird/wrap/internal/state"
	"github.com/sarcasticbird/wrap/internal/terms"
	"github.com/sarcasticbird/wrap/internal/tmux"
	"github.com/sarcasticbird/wrap/internal/tree"
	"github.com/sarcasticbird/wrap/internal/workspace"
	"github.com/sarcasticbird/wrap/internal/workspaces"
	"github.com/sarcasticbird/wrap/internal/workspacesui"
)

// version is stamped by the release build via
// -ldflags "-X main.version=<tag>". A `go install` never passes ldflags, so
// fall back to the version the module was installed at — otherwise every
// installed binary reports "dev" forever with no way to tell them apart.
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	// A build inside a VCS checkout is stamped with a pseudo-version
	// (v0.0.0-<time>-<commit>, plus +dirty for uncommitted changes), which
	// is more useful than "dev". "(devel)" means the toolchain had nothing
	// to stamp — keep "dev" rather than print that.
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		version = v
	}
}

// wsArg extracts the required workspace argument for a pane subcommand.
// Pane subcommands are internal — wrap itself always supplies the ws
// arg when it spawns them — so a missing arg means someone ran the
// subcommand directly.
func wsArg(args []string) (string, error) {
	if len(args) <= 1 || args[1] == "" {
		return "", errors.New("internal command — run wrap")
	}
	ws := args[1]
	if err := config.ValidateWorkspaceName(ws); err != nil {
		return "", fmt.Errorf("internal workspace: %w", err)
	}
	return ws, nil
}

// dispatchPane resolves the ws arg and runs fn, or reports the missing
// ws arg as an error.
func dispatchPane(args []string, fn func(string) error) error {
	ws, err := wsArg(args)
	if err != nil {
		return err
	}
	return fn(ws)
}

func dispatchTUIAttach(args []string, fn func(string, string) error) error {
	if len(args) != 3 || args[1] == "" || args[2] == "" {
		return errors.New("internal command — run wrap tui")
	}
	return fn(args[1], args[2])
}

func main() {
	err := runArgs(os.Args[1:], commandFuncs{
		sidebar:   runSidebar,
		watch:     runWatch,
		attach:    runAttach,
		tui:       runTUI,
		tuiAttach: runTUIAttach,
		launch:    runLaunch,
		version:   func() { fmt.Println("wrap", version) },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "wrap:", err)
		os.Exit(1)
	}
}

type commandFuncs struct {
	sidebar   func(string) error
	watch     func(string) error
	attach    func(string) error
	tui       func() error
	tuiAttach func(string, string) error
	launch    func(string) error
	version   func()
}

func runArgs(args []string, funcs commandFuncs) error {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "sidebar":
		return dispatchPane(args, funcs.sidebar)
	case "watch":
		return dispatchPane(args, funcs.watch)
	case "attach":
		return dispatchPane(args, funcs.attach)
	case "tui":
		return funcs.tui()
	case "tui-attach":
		return dispatchTUIAttach(args, funcs.tuiAttach)
	case "version":
		funcs.version()
		return nil
	default:
		// "" or a directory: launch that workspace.
		return funcs.launch(cmd)
	}
}

func discoverFn(cfg *config.Config) func(string) ([]gitx.Discovered, []string, error) {
	depth := 1
	if cfg != nil {
		depth = cfg.WalkDepth
	}
	return func(dir string) ([]gitx.Discovered, []string, error) {
		return gitx.Discover(dir, depth)
	}
}

type workspaceGuard func(*launcher.Manager, state.Meta) (func() error, error)

func runLaunch(target string) error {
	if err := tmux.CheckVersion(tmux.NewServer(tmux.SocketUI).R); err != nil {
		return err
	}
	cfg, cfgWarns, err := loadConfigOptional()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	ws, discoveryWarns, err := workspace.Resolve(target, cwd, cfg, discoverFn(cfg))
	if err != nil {
		return err
	}
	return runResolvedWorkspace(ws, cwd, cfgWarns, discoveryWarns,
		func(m *launcher.Manager, meta state.Meta) (func() error, error) {
			return m.GuardWorkspaceMeta(meta)
		})
}

func runResolvedWorkspace(ws *workspace.Workspace, cwd string, cfgWarns, discoveryWarns []string,
	guard workspaceGuard,
) (retErr error) {
	entryNames, entryPaths, entryWarn := tree.EntryNamesAndPaths(ws.Root, ws.Name, ws.Repos)
	topologyComplete, topologyDetail := migrationTopology(discoveryWarns, entryWarn)
	warns := append([]string{}, cfgWarns...)
	warns = append(warns, discoveryWarns...)
	if entryWarn != "" {
		warns = append(warns, entryWarn)
	}
	for _, warn := range warns {
		fmt.Fprintln(os.Stderr, "wrap:", warn)
	}
	m, err := launcher.New(ws.Name)
	if err != nil {
		return err
	}
	releaseOwnership, err := guard(m, ws.Meta())
	if err != nil {
		return err
	}
	// Idempotent: error paths release here; the success path releases
	// explicitly once LaunchUI makes this ownership visible to contenders.
	ownershipReleased := false
	defer func() {
		if !ownershipReleased {
			retErr = errors.Join(retErr, releaseOwnership())
		}
	}()
	if err := m.EnsureSessionServer(cwd); err != nil {
		return err
	}
	if err := m.MigrateLegacyEntrySessionsWithPaths(entryNames, entryPaths, topologyComplete); err != nil {
		if topologyDetail != "" {
			return fmt.Errorf("%w (entry discovery: %s)", err, topologyDetail)
		}
		return err
	}
	if err := m.ValidateLiveEntrySessions(entryPaths, topologyComplete); err != nil {
		if topologyDetail != "" {
			return fmt.Errorf("%w (entry discovery: %s)", err, topologyDetail)
		}
		return err
	}
	topology, err := topologyFingerprint(entryPaths)
	if err != nil {
		return err
	}
	// A fresh workspace opens at its root, not at wrap-home. Re-seed when
	// the previous root session died (e.g. its command failed to start).
	sel, cmd := ws.InitialSelection()
	needsSeed, err := selectionNeedsSeed(ws.Name, entryPaths, m.Sess.HasSession)
	if err != nil {
		return err
	}
	if needsSeed {
		if err := m.EnsureEntrySession(sel.Session, sel.Path, cmd); err != nil {
			return err
		}
		if err := state.Write(ws.Name, sel); err != nil {
			return err
		}
	}
	params := state.ChromeParams{
		TreeSide:  ws.TreeSide,
		TreeWidth: ws.TreeWidth,
		Keys:      ws.Keys,
		Cmd:       ws.Cmd,
		WalkDepth: ws.WalkDepth,
		Topology:  topology,
	}
	if err := m.LaunchUI(params); err != nil {
		return err
	}
	releaseErr := releaseOwnership()
	ownershipReleased = true
	if releaseErr != nil {
		return releaseErr
	}
	return m.AttachUI()
}

func topologyFingerprint(paths map[string]string) (string, error) {
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	hash := sha256.New()
	for _, name := range names {
		abs, err := filepath.Abs(paths[name])
		if err != nil {
			return "", fmt.Errorf("topology path %s: %w", paths[name], err)
		}
		canonical, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return "", fmt.Errorf("topology path %s: %w", abs, err)
		}
		if _, err := fmt.Fprintf(hash, "%d:%s%d:%s", len(name), name, len(canonical), canonical); err != nil {
			return "", fmt.Errorf("fingerprint topology: %w", err)
		}
	}
	return fmt.Sprintf("%x", hash.Sum(nil)), nil
}

func migrationTopology(discoveryWarns []string, entryWarn string) (bool, string) {
	detail := append([]string{}, discoveryWarns...)
	if entryWarn != "" {
		detail = append(detail, entryWarn)
	}
	return len(detail) == 0, strings.Join(detail, "; ")
}

func selectionNeedsSeed(ws string, entryPaths map[string]string, has func(string) (bool, error)) (bool, error) {
	cur, exists, err := state.Read(ws)
	if err != nil {
		return false, fmt.Errorf("read selection for workspace %q while deciding whether to seed: %w", ws, err)
	}
	if !exists {
		return true, nil
	}
	// A current repository/worktree row is intentionally selectable before
	// it has a terminal: the terms pane uses that persisted path when the
	// user presses n. A removed, renamed, or path-mismatched row is stale
	// and must fall back to the workspace root.
	if cur.Session != ws {
		expected, ok := entryPaths[cur.Session]
		if !ok || cur.Path == "" {
			return true, nil
		}
		abs, err := filepath.Abs(cur.Path)
		if err != nil {
			return true, nil
		}
		actual, err := filepath.EvalSymlinks(abs)
		if err != nil {
			return true, nil
		}
		expected, err = filepath.EvalSymlinks(expected)
		if err != nil {
			return false, fmt.Errorf("resolve selected entry path %q while deciding whether to seed workspace %q: %w", expected, ws, err)
		}
		return actual != expected, nil
	}
	alive, err := has(cur.Session)
	if err != nil {
		return false, fmt.Errorf("check selected session %q for workspace %q while deciding whether to seed: %w", cur.Session, ws, err)
	}
	return !alive, nil
}

func runSidebar(ws string) error {
	w, warns, err := resolveWS(ws)
	if err != nil {
		return err
	}
	m, err := launcher.New(ws)
	if err != nil {
		return err
	}
	return tree.Run(m, tree.Options{
		WS:       ws,
		Root:     w.Root,
		RootName: filepath.Base(w.Root),
		Cmd:      w.Cmd,
		Keys:     w.Keys.WithDefaults(),
		Note:     strings.Join(warns, "; "),
		Repos:    w.Repos,
	})
}

func runWatch(ws string) error {
	w, _, err := resolveWS(ws)
	if err != nil {
		return err
	}
	m, err := launcher.New(ws)
	if err != nil {
		return err
	}
	return terms.Run(m, terms.Options{
		WS: ws, Root: w.Root, Cmd: w.Cmd,
		Keys: w.Keys.WithDefaults(),
	})
}

func runAttach(ws string) error {
	m, err := launcher.New(ws)
	if err != nil {
		return err
	}
	return m.RunMiddleAttach()
}

type selectorFunc func(initialNote string) (workspaces.Workspace, bool, error)

func runTUILoop(selectWorkspace selectorFunc,
	launch func(workspaces.Workspace) (string, error),
) error {
	initialNote := ""
	for {
		selected, ok, err := selectWorkspace(initialNote)
		if err != nil {
			return fmt.Errorf("run workspace selector: %w", err)
		}
		if !ok {
			return nil
		}
		note, err := launch(selected)
		if err != nil {
			initialNote = err.Error()
			continue
		}
		initialNote = note
	}
}

func runTUI() error {
	source := workspaces.NewRuntimeSource()
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate wrap binary for workspace selector: %w", err)
	}
	return runTUILoop(func(initialNote string) (workspaces.Workspace, bool, error) {
		return workspacesui.Run(workspacesui.Options{
			Discover: func() (workspaces.Snapshot, error) {
				return workspaces.Discover(source)
			},
			InitialNote: initialNote,
		})
	}, func(selected workspaces.Workspace) (string, error) {
		return runSelectedWrapChild(executable, selected)
	})
}

func runTUIAttach(name, root string) error {
	if err := tmux.CheckVersion(tmux.NewServer(tmux.SocketUI).R); err != nil {
		return err
	}
	cfg, cfgWarns, err := loadConfigOptional()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	ws, discoveryWarns, err := resolveSelectedWorkspace(
		workspaces.Workspace{Name: name, Root: root},
		cfg,
		discoverFn(cfg),
	)
	if err != nil {
		return err
	}
	return runResolvedWorkspace(ws, cwd, cfgWarns, discoveryWarns,
		func(m *launcher.Manager, meta state.Meta) (func() error, error) {
			return m.GuardActiveWorkspaceMeta(meta)
		})
}

func resolveSelectedWorkspace(selected workspaces.Workspace, cfg *config.Config,
	discover func(string) ([]gitx.Discovered, []string, error),
) (*workspace.Workspace, []string, error) {
	if !filepath.IsAbs(selected.Root) {
		return nil, nil, fmt.Errorf("selected workspace %q root %q is not absolute", selected.Name, selected.Root)
	}
	canonical, err := filepath.EvalSymlinks(selected.Root)
	if err != nil {
		return nil, nil, fmt.Errorf("selected workspace %q root %q: %w",
			selected.Name, selected.Root, err)
	}
	if canonical != filepath.Clean(selected.Root) {
		return nil, nil, fmt.Errorf(
			"selected workspace %q root changed since selector refresh: %q now resolves to %q",
			selected.Name, selected.Root, canonical,
		)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return nil, nil, fmt.Errorf("selected workspace %q root %q: %w",
			selected.Name, canonical, err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("selected workspace %q root %q is not a directory",
			selected.Name, canonical)
	}
	return workspace.FromMeta(
		selected.Name,
		state.Meta{Kind: "folder", Root: selected.Root},
		cfg,
		discover,
	)
}

func runSelectedWrapChild(executable string, selected workspaces.Workspace) (string, error) {
	cmd := exec.Command(executable, "tui-attach", selected.Name, selected.Root)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return "", fmt.Errorf("launch workspace %s at %s: %s: %w",
				selected.Name, selected.Root, detail, err)
		}
		return "", fmt.Errorf("launch workspace %s at %s: %w", selected.Name, selected.Root, err)
	}
	return strings.TrimSpace(stderr.String()), nil
}

// loadConfigOptional: absent file is fine (nil config); a broken file
// is still an error — never silently ignore a config the user wrote.
func loadConfigOptional() (*config.Config, []string, error) {
	path, err := config.DefaultPath()
	if err != nil {
		return nil, nil, err
	}
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return nil, nil, nil
	}
	return config.Load(path)
}

// resolveWS reconstructs a pane subprocess's workspace from its meta.
// The meta is always written by runLaunch's GuardWorkspaceMeta before a
// pane subcommand is ever spawned, so a missing meta means the ws name
// hasn't been launched (or its state was wiped) — surface that clearly
// rather than guessing at a workspace.
func resolveWS(ws string) (*workspace.Workspace, []string, error) {
	cfg, cfgWarns, err := loadConfigOptional()
	if err != nil {
		return nil, nil, err
	}
	meta, ok, err := state.ReadMeta(ws)
	if err != nil {
		return nil, nil, err
	}
	if !ok {
		return nil, nil, fmt.Errorf("no workspace state for %q — run wrap to launch it first", ws)
	}
	w, warns, err := workspace.FromMeta(ws, meta, cfg, discoverFn(cfg))
	return w, append(cfgWarns, warns...), err
}
