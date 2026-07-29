// Package state persists a workspace's current selection to a small JSON
// file that the panes poll. This is wrap's entire IPC.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/sarcasticbird/wrap/internal/config"
)

// Selection is the workspace's current pick: which tree row is active and
// which tmux session backs it. Written by the tree pane, read by the terms
// pane and by launch.
type Selection struct {
	Entry   string `json:"entry"`
	Session string `json:"session"`
	Path    string `json:"path"`
}

// rootDir returns wrap's shared state directory:
// $XDG_STATE_HOME/wrap, falling back to ~/.local/state/wrap.
func rootDir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "wrap"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("state: home dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "wrap"), nil
}

// dir returns this workspace's state directory.
func dir(ws string) (string, error) {
	root, err := rootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, ws), nil
}

func filePath(ws string) (string, error) {
	d, err := dir(ws)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "current"), nil
}

// LockWorkspace serializes launch, session mutation, selection writes, and
// shutdown for one workspace name. The lock is advisory and process-scoped
// through a file in the same state directory as workspace.json.
func LockWorkspace(ws string) (func() error, error) {
	d, err := dir(ws)
	if err != nil {
		return nil, err
	}
	return lockFile(filepath.Join(d, "launch.lock"), "workspace "+ws)
}

// LockUIServer serializes mutations of options and key bindings that are
// global to wrap's shared UI tmux server. Workspace locks cannot provide this
// guarantee because concurrent launches use different workspace names.
func LockUIServer() (func() error, error) {
	root, err := rootDir()
	if err != nil {
		return nil, err
	}
	return lockFile(filepath.Join(root, "ui-server.lock"), "UI server")
}

func lockFile(path, subject string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("state: mkdir %s for %s lock: %w", filepath.Dir(path), subject, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: open %s lock %s: %w", subject, path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, errors.Join(
			fmt.Errorf("state: lock %s: %w", subject, err),
			f.Close(),
		)
	}
	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			releaseErr = errors.Join(
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN),
				f.Close(),
			)
			if releaseErr != nil {
				releaseErr = fmt.Errorf("state: unlock %s: %w", subject, releaseErr)
			}
		})
		return releaseErr
	}
	return release, nil
}

func shutdownPath(ws string) (string, error) {
	d, err := dir(ws)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "shutting-down"), nil
}

// BeginShutdown publishes a durable barrier while the workspace lock is held.
// Pane processes that were already waiting on the lock refuse mutation once
// they acquire it, so they cannot recreate sessions after the shutdown sweep.
func BeginShutdown(ws string) error {
	path, err := shutdownPath(ws)
	if err != nil {
		return err
	}
	return writeAtomic(path, []byte("1\n"))
}

func ClearShutdown(ws string) error {
	path, err := shutdownPath(ws)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("state: clear shutdown barrier %s: %w", path, err)
	}
	return nil
}

func IsShuttingDown(ws string) (bool, error) {
	path, err := shutdownPath(ws)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, fmt.Errorf("state: inspect shutdown barrier %s: %w", path, err)
}

// writeAtomic writes b to path via same-dir tmp + rename.
//
// The temp file gets a unique name rather than a fixed path+".tmp": both
// side panes write state, so two writers sharing one temp path could
// interleave their bytes into it and rename the resulting mixture — valid
// JSON is not guaranteed to survive being spliced with another document.
// A unique file per write means each rename publishes exactly what its
// caller wrote.
func writeAtomic(path string, b []byte) (retErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("state: mkdir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("state: temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer func() {
		if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
			retErr = errors.Join(retErr, fmt.Errorf("state: remove temp %s: %w", tmp, err))
		}
	}()
	if _, err := f.Write(b); err != nil {
		writeErr := fmt.Errorf("state: write %s: %w", tmp, err)
		if closeErr := f.Close(); closeErr != nil {
			return errors.Join(writeErr, fmt.Errorf("state: close %s after write failure: %w", tmp, closeErr))
		}
		return writeErr
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("state: close %s: %w", tmp, err)
	}
	// CreateTemp's 0600 is kept deliberately: this is per-user state under
	// $XDG_STATE_HOME and nothing else needs to read it. Widening it with
	// an explicit chmod would also override a restrictive umask, which the
	// previous os.WriteFile path respected.
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("state: rename %s: %w", path, err)
	}
	return nil
}

func Write(ws string, s Selection) error {
	p, err := filePath(ws)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal selection: %w", err)
	}
	return writeAtomic(p, b)
}

// ClearSelection removes the workspace's persisted selection file. A
// workspace that never had one (or already cleared it) is not an error.
func ClearSelection(ws string) error {
	p, err := filePath(ws)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("state: clear %s: %w", p, err)
	}
	return nil
}

func Read(ws string) (Selection, bool, error) {
	var s Selection
	p, err := filePath(ws)
	if err != nil {
		return s, false, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return s, false, nil
	}
	if err != nil {
		return s, false, fmt.Errorf("state: read %s: %w", p, err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, false, fmt.Errorf("state: unmarshal %s: %w", p, err)
	}
	return s, true, nil
}

func entryPathsPath(ws string) (string, error) {
	d, err := dir(ws)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "entry-paths.json"), nil
}

// WriteEntryPaths records the launch-discovered session-to-directory mapping.
// Pane subprocesses use it to revalidate a live entry immediately before
// switching or automatically attaching to it.
func WriteEntryPaths(ws string, paths map[string]string) error {
	p, err := entryPathsPath(ws)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(paths, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal entry paths: %w", err)
	}
	return writeAtomic(p, b)
}

func ReadEntryPaths(ws string) (map[string]string, bool, error) {
	p, err := entryPathsPath(ws)
	if err != nil {
		return nil, false, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("state: read %s: %w", p, err)
	}
	var paths map[string]string
	if err := json.Unmarshal(b, &paths); err != nil {
		return nil, false, fmt.Errorf("state: unmarshal %s: %w", p, err)
	}
	return paths, true, nil
}

// Meta records what a workspace IS, so pane subprocesses can
// reconstruct it without re-deriving CLI context.
type Meta struct {
	Kind string `json:"kind"` // always "folder"; kept for state-file compatibility
	Root string `json:"root"` // folder workspaces only
}

func metaPath(ws string) (string, error) {
	d, err := dir(ws)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "workspace.json"), nil
}

func WriteMeta(ws string, m Meta) error {
	p, err := metaPath(ws)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal meta: %w", err)
	}
	return writeAtomic(p, b)
}

func ReadMeta(ws string) (Meta, bool, error) {
	var m Meta
	p, err := metaPath(ws)
	if err != nil {
		return m, false, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return m, false, nil
	}
	if err != nil {
		return m, false, fmt.Errorf("state: read %s: %w", p, err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, false, fmt.Errorf("state: unmarshal %s: %w", p, err)
	}
	return m, true, nil
}

// ChromeBuild is the schema version of the chrome wrap knows how to build.
// Bump it whenever a release changes what the chrome must look like at
// build time — new tmux options set during construction, or new behavior
// in the pane subprocesses themselves.
//
// The panes are long-lived processes started when the chrome was built, so
// they keep running whatever binary was on disk that day. Upgrading wrap
// therefore does NOT reach an already-running workspace: without this
// version, a matching ChromeParams made LaunchUI reuse chrome built by an
// older binary indefinitely, and the new feature simply never appeared.
// That is not a theoretical staleness — it shipped, and the bell work was
// invisible in every workspace that happened to already be open.
//
// Old chrome.json files have no "build" key and decode to 0, so any
// workspace built before this existed rebuilds on the next launch.
const ChromeBuild = 12

// ChromeParams records what the live chrome was BUILT with: the launch
// params LaunchUI used for its splits/keys, the entry topology fingerprint,
// and two config values that don't shape the build itself (Cmd, WalkDepth)
// but that pane subprocesses bake in at spawn. A change to any of them still
// needs the chrome (and its panes) torn down and rebuilt.
// wrap compares a freshly-resolved ChromeParams against this stored one on
// every run; any difference triggers a rebuild before reattaching.
type ChromeParams struct {
	Build     int         `json:"build"`
	TreeSide  string      `json:"tree_side"`
	TreeWidth int         `json:"tree_width"`
	Keys      config.Keys `json:"keys"`
	Cmd       string      `json:"cmd"`
	WalkDepth int         `json:"walk_depth"`
	Topology  string      `json:"topology"`
}

func chromeParamsPath(ws string) (string, error) {
	d, err := dir(ws)
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "chrome.json"), nil
}

// WriteChromeParams persists the params the live chrome was just built
// with, so a later run can detect a config change and rebuild.
func WriteChromeParams(ws string, p ChromeParams) error {
	path, err := chromeParamsPath(ws)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return fmt.Errorf("state: marshal chrome params: %w", err)
	}
	return writeAtomic(path, b)
}

// ReadChromeParams reads back the params the live chrome was built with.
// ok is false when no chrome.json exists yet (a workspace launched before
// this feature existed, or never launched at all) — not an error.
func ReadChromeParams(ws string) (ChromeParams, bool, error) {
	var p ChromeParams
	path, err := chromeParamsPath(ws)
	if err != nil {
		return p, false, err
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("state: read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, false, fmt.Errorf("state: unmarshal %s: %w", path, err)
	}
	return p, true, nil
}
