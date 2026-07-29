package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/gitx"
	"github.com/sarcasticbird/wrap/internal/state"
)

// The pane subcommands are internal — wrap supplies the ws argument when it
// spawns them — so a missing one means a human ran the subcommand directly
// and should be told, not handed a workspace named "".
func TestWsArg(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		want    string
		wantErr bool
	}{
		{"present", []string{"sidebar", "myws"}, "myws", false},
		{"missing", []string{"sidebar"}, "", true},
		{"empty string", []string{"sidebar", ""}, "", true},
		{"extra args ignored", []string{"sidebar", "myws", "junk"}, "myws", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := wsArg(tc.args)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("ws = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTopologyFingerprintIsOrderIndependentAndPathSensitive(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	a, err := topologyFingerprint(map[string]string{"b": second, "a": first})
	if err != nil {
		t.Fatal(err)
	}
	b, err := topologyFingerprint(map[string]string{"a": first, "b": second})
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("map iteration changed fingerprint: %q != %q", a, b)
	}
	changed, err := topologyFingerprint(map[string]string{"a": first, "b": first})
	if err != nil {
		t.Fatal(err)
	}
	if changed == a {
		t.Fatal("changed topology retained the same fingerprint")
	}
}

func TestWsArgRejectsUnsafeWorkspaceNames(t *testing.T) {
	for _, name := range []string{"../other", "a/b", `a\b`, ".", "..", config.HomeSession} {
		if _, err := wsArg([]string{"sidebar", name}); err == nil {
			t.Errorf("wsArg(%q) = nil error", name)
		}
	}
}

func TestSelectionNeedsSeedSurfacesMalformedState(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	dir := filepath.Join(stateHome, "wrap", "ws")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "current"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := selectionNeedsSeed("ws", nil, func(string) (bool, error) { return false, nil }); err == nil {
		t.Fatal("malformed selection was treated as absent")
	} else if got := err.Error(); !strings.Contains(got, "ws") || !strings.Contains(got, "seed") {
		t.Fatalf("err = %q, want workspace and seeding context", got)
	}
}

func TestSelectionNeedsSeedSurfacesSessionCheckFailure(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.Write("ws", state.Selection{Entry: "ws", Session: "ws", Path: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	_, err := selectionNeedsSeed("ws", nil, func(string) (bool, error) {
		return false, errors.New("tmux permission denied")
	})
	if err == nil || !strings.Contains(err.Error(), "tmux permission denied") {
		t.Fatalf("session check failure was treated as absence: %v", err)
	}
}

func TestSelectionNeedsSeedPreservesSessionlessRepository(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	if err := state.Write("ws", state.Selection{
		Entry:   "repo",
		Session: "ws/repo",
		Path:    repo,
	}); err != nil {
		t.Fatal(err)
	}
	called := false
	needs, err := selectionNeedsSeed("ws", map[string]string{"ws/repo": repo}, func(string) (bool, error) {
		called = true
		return false, nil
	})
	if err != nil || needs {
		t.Fatalf("session-less repository selection needsSeed=%v err=%v, want preserved", needs, err)
	}
	if called {
		t.Error("repository selection existence was probed even though only a missing root should trigger seeding")
	}
}

func TestSelectionNeedsSeedResetsStaleRepositorySelection(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entryPaths map[string]string
	}{
		{name: "removed", entryPaths: map[string]string{}},
		{name: "path changed", entryPaths: map[string]string{"ws/repo": t.TempDir()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			old := t.TempDir()
			if err := state.Write("ws", state.Selection{Entry: "repo", Session: "ws/repo", Path: old}); err != nil {
				t.Fatal(err)
			}
			needs, err := selectionNeedsSeed("ws", tc.entryPaths, func(string) (bool, error) {
				t.Fatal("stale non-root selection should not probe a session")
				return false, nil
			})
			if err != nil || !needs {
				t.Fatalf("stale selection needsSeed=%v err=%v, want true", needs, err)
			}
		})
	}
}

func TestSelectionNeedsSeedRecreatesMissingRoot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := state.Write("ws", state.Selection{
		Entry:   "ws",
		Session: "ws",
		Path:    "/workspace",
	}); err != nil {
		t.Fatal(err)
	}
	needs, err := selectionNeedsSeed("ws", nil, func(name string) (bool, error) {
		if name != "ws" {
			t.Fatalf("checked %q, want root session ws", name)
		}
		return false, nil
	})
	if err != nil || !needs {
		t.Fatalf("missing root needsSeed=%v err=%v, want true", needs, err)
	}
}

func TestMigrationTopologyIncludesInitialDiscoveryWarnings(t *testing.T) {
	complete, detail := migrationTopology([]string{"private: permission denied"}, "")
	if complete {
		t.Fatal("partial workspace discovery was treated as complete")
	}
	if !strings.Contains(detail, "private: permission denied") {
		t.Fatalf("detail = %q, want initial discovery warning", detail)
	}
}

func TestMigrationTopologyIncludesWorktreeWarnings(t *testing.T) {
	complete, detail := migrationTopology(nil, "worktrees api: corrupt metadata")
	if complete {
		t.Fatal("partial worktree discovery was treated as complete")
	}
	if !strings.Contains(detail, "worktrees api: corrupt metadata") {
		t.Fatalf("detail = %q, want worktree warning", detail)
	}
}

func TestDispatchPaneRunsWithResolvedWS(t *testing.T) {
	var called string
	err := dispatchPane([]string{"sidebar", "myws"}, func(ws string) error {
		called = ws
		return nil
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if called != "myws" {
		t.Errorf("fn received %q, want myws", called)
	}
}

// A missing ws must not reach fn at all — running a pane subcommand against
// an unnamed workspace is what the guard exists to prevent.
func TestDispatchPaneDoesNotRunWithoutWS(t *testing.T) {
	called := false
	err := dispatchPane([]string{"sidebar"}, func(string) error {
		called = true
		return nil
	})
	if err == nil {
		t.Error("missing ws should error")
	}
	if called {
		t.Error("fn ran despite the missing ws")
	}
}

func TestDispatchPanePropagatesError(t *testing.T) {
	want := "boom"
	err := dispatchPane([]string{"watch", "myws"}, func(string) error {
		return errFake(want)
	})
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

type errFake string

func (e errFake) Error() string { return string(e) }

// discoverFn threads walk_depth through to the walker. A nil config must
// still yield a usable depth rather than 0, which would find nothing.
func TestDiscoverFnThreadsWalkDepth(t *testing.T) {
	root := t.TempDir()
	mustInitRepo(t, filepath.Join(root, "top"))
	if err := os.MkdirAll(filepath.Join(root, "group"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustInitRepo(t, filepath.Join(root, "group", "deep"))

	// nil config → depth 1: the nested repo stays hidden.
	found, _, err := discoverFn(nil)(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "top" {
		t.Errorf("nil config should walk one level, got %+v", names(found))
	}

	// walk_depth 2 → the nested repo appears.
	found, _, err = discoverFn(&config.Config{WalkDepth: 2})(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Errorf("walk_depth 2 should reach the nested repo, got %+v", names(found))
	}
}

func names(ds []gitx.Discovered) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.Name
	}
	return out
}

// An absent config is a supported setup, not an error.
func TestLoadConfigOptionalAbsentFileIsFine(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, warns, err := loadConfigOptional()
	if err != nil {
		t.Fatalf("absent config errored: %v", err)
	}
	if cfg != nil {
		t.Errorf("cfg = %+v, want nil for an absent file", cfg)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %v", warns)
	}
}

// A config the user actually wrote but got wrong must never be silently
// ignored — that failure mode cost real debugging time before.
func TestLoadConfigOptionalBrokenFileErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, "this is not = valid toml [[[\n")
	if _, _, err := loadConfigOptional(); err == nil {
		t.Error("a malformed config should error, not be ignored")
	}
}

func TestLoadConfigOptionalReadsValuesAndWarnsOnUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, "tree_side = \"right\"\ntree_width = 15\nnot_a_key = 1\n")
	cfg, warns, err := loadConfigOptional()
	if err != nil {
		t.Fatal(err)
	}
	if cfg == nil || cfg.TreeSide != "right" || cfg.TreeWidth != 15 {
		t.Errorf("cfg = %+v, want right/15", cfg)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "not_a_key") {
		t.Errorf("warns = %v, want one naming not_a_key", warns)
	}
}

// A pane subprocess reconstructs its workspace from meta that runLaunch
// always writes first. No meta means the name was never launched — say so
// rather than inventing a workspace.
func TestResolveWSWithoutMetaExplainsItself(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	_, _, err := resolveWS("never-launched")
	if err == nil {
		t.Fatal("missing meta should error")
	}
	if !strings.Contains(err.Error(), "never-launched") || !strings.Contains(err.Error(), "run wrap") {
		t.Errorf("err = %q, want it to name the workspace and say what to do", err)
	}
}

func TestResolveWSFromMeta(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	root := t.TempDir()
	mustInitRepo(t, filepath.Join(root, "repo-a"))
	if err := state.WriteMeta("ws", state.Meta{Kind: "folder", Root: root}); err != nil {
		t.Fatal(err)
	}

	w, _, err := resolveWS("ws")
	if err != nil {
		t.Fatal(err)
	}
	if w.Root != root {
		t.Errorf("Root = %q, want %q", w.Root, root)
	}
	if w.Name != "ws" {
		t.Errorf("Name = %q, want ws", w.Name)
	}
	if len(w.Repos) != 1 || w.Repos[0].Name != "repo-a" {
		t.Errorf("Repos = %+v", names(w.Repos))
	}
}

// A broken config must surface through the pane path too, not just launch.
//
// The workspace metadata is written, and the returned error is matched
// against the offending value rather than merely being non-nil. Without
// both, resolveWS would fail on the MISSING metadata instead, and the test
// would pass just as happily against a build that dropped config errors
// on the floor — which is the only thing it exists to catch.
func TestResolveWSSurfacesConfigError(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfgDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfgDir)

	root := t.TempDir()
	mustInitRepo(t, filepath.Join(root, "repo-a"))
	if err := state.WriteMeta("ws", state.Meta{Kind: "folder", Root: root}); err != nil {
		t.Fatal(err)
	}
	// Guard the premise: with a good config this workspace resolves, so a
	// failure below can only come from the config.
	if _, _, err := resolveWS("ws"); err != nil {
		t.Fatalf("workspace does not resolve even with a valid config: %v", err)
	}

	writeConfig(t, cfgDir, "tree_side = \"sideways\"\n") // valid TOML, invalid value
	_, _, err := resolveWS("ws")
	if err == nil {
		t.Fatal("an invalid config value should surface to the pane subcommand")
	}
	if !strings.Contains(err.Error(), "tree_side") || !strings.Contains(err.Error(), "sideways") {
		t.Errorf("err = %q, want it to name tree_side and the rejected value", err)
	}
}

func writeConfig(t *testing.T, xdgConfigHome, body string) {
	t.Helper()
	dir := filepath.Join(xdgConfigHome, "wrap")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wrap.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustInitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}
