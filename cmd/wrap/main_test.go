package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/gitx"
	"github.com/sarcasticbird/wrap/internal/mirror"
	"github.com/sarcasticbird/wrap/internal/state"
	"github.com/sarcasticbird/wrap/internal/workspaces"
)

func TestMirrorDiagnosticsUseWorkspaceStatePath(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	sink := mirrorDiagnostics("api")
	if err := sink.Write(mirror.DiagnosticRecord{
		Level: "info", Component: "server", Event: "started",
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stateHome, "wrap", "api", "mirror.log")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"event":"started"`) {
		t.Fatalf("workspace mirror log = %q", data)
	}
}

func TestRunArgsDispatchesTUICommand(t *testing.T) {
	var tuiCalls int
	var launches []string
	err := runArgs([]string{"tui"}, commandFuncs{
		tui: func() error {
			tuiCalls++
			return nil
		},
		launch: func(target string) error {
			launches = append(launches, target)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tuiCalls != 1 || len(launches) != 0 {
		t.Fatalf("tui calls = %d, launches = %v", tuiCalls, launches)
	}
}

func TestRunArgsTreatsExplicitTUIDirectoryAsLaunchTarget(t *testing.T) {
	var tuiCalls int
	var launches []string
	err := runArgs([]string{"./tui"}, commandFuncs{
		tui: func() error {
			tuiCalls++
			return nil
		},
		launch: func(target string) error {
			launches = append(launches, target)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if tuiCalls != 0 || len(launches) != 1 || launches[0] != "./tui" {
		t.Fatalf("tui calls = %d, launches = %v", tuiCalls, launches)
	}
}

func TestRunArgsDispatchesSelectedWorkspaceIdentity(t *testing.T) {
	var gotName, gotRoot string
	err := runArgs([]string{"tui-attach", "alias", "/real/service"}, commandFuncs{
		tuiAttach: func(name, root string) error {
			gotName, gotRoot = name, root
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "alias" || gotRoot != "/real/service" {
		t.Fatalf("selected identity = %q, %q", gotName, gotRoot)
	}
}

func TestRunTUILoopReturnsToSelectorAfterDetach(t *testing.T) {
	selections := []struct {
		workspace workspaces.Workspace
		ok        bool
	}{
		{workspace: workspaces.Workspace{Name: "alias", Root: "/real/alpha"}, ok: true},
		{workspace: workspaces.Workspace{Name: "beta", Root: "/work/beta"}, ok: true},
		{},
	}
	var selectorCalls int
	var launched []workspaces.Workspace
	err := runTUILoop(func(initialNote string) (workspaces.Workspace, bool, error) {
		if initialNote != "" {
			t.Errorf("selector note = %q, want empty", initialNote)
		}
		selection := selections[selectorCalls]
		selectorCalls++
		return selection.workspace, selection.ok, nil
	}, func(selected workspaces.Workspace) (string, error) {
		launched = append(launched, selected)
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []workspaces.Workspace{
		{Name: "alias", Root: "/real/alpha"},
		{Name: "beta", Root: "/work/beta"},
	}
	if !reflect.DeepEqual(launched, want) {
		t.Fatalf("launched = %v", launched)
	}
	if selectorCalls != 3 {
		t.Fatalf("selector calls = %d, want 3", selectorCalls)
	}
}

func TestRunTUILoopReturnsLaunchFailureAsNextInitialNote(t *testing.T) {
	var selectorCalls int
	var launchCalls int
	err := runTUILoop(func(initialNote string) (workspaces.Workspace, bool, error) {
		selectorCalls++
		switch selectorCalls {
		case 1:
			if initialNote != "" {
				t.Fatalf("first selector note = %q", initialNote)
			}
			return workspaces.Workspace{Name: "bad", Root: "/work/bad"}, true, nil
		case 2:
			if initialNote != "cannot launch" {
				t.Fatalf("second selector note = %q", initialNote)
			}
			return workspaces.Workspace{Name: "good", Root: "/work/good"}, true, nil
		case 3:
			if initialNote != "" {
				t.Fatalf("third selector note = %q, want cleared", initialNote)
			}
			return workspaces.Workspace{}, false, nil
		default:
			t.Fatalf("unexpected selector call %d", selectorCalls)
			return workspaces.Workspace{}, false, nil
		}
	}, func(selected workspaces.Workspace) (string, error) {
		launchCalls++
		if selected.Name == "bad" {
			return "", errors.New("cannot launch")
		}
		return "", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if launchCalls != 2 {
		t.Fatalf("launch calls = %d, want 2", launchCalls)
	}
}

func TestRunTUILoopReturnsSelectorFailure(t *testing.T) {
	want := errors.New("selector failed")
	err := runTUILoop(func(string) (workspaces.Workspace, bool, error) {
		return workspaces.Workspace{}, false, want
	}, func(workspaces.Workspace) (string, error) {
		t.Fatal("launch called after selector failure")
		return "", nil
	})
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want selector failure", err)
	}
}

func TestRunTUILoopShowsSuccessfulLaunchWarningsOnReturn(t *testing.T) {
	var selectorCalls int
	err := runTUILoop(func(initialNote string) (workspaces.Workspace, bool, error) {
		selectorCalls++
		switch selectorCalls {
		case 1:
			if initialNote != "" {
				t.Fatalf("first selector note = %q", initialNote)
			}
			return workspaces.Workspace{Name: "alpha", Root: "/work/alpha"}, true, nil
		case 2:
			if initialNote != "repository discovery incomplete" {
				t.Fatalf("second selector note = %q", initialNote)
			}
			return workspaces.Workspace{}, false, nil
		default:
			t.Fatalf("unexpected selector call %d", selectorCalls)
			return workspaces.Workspace{}, false, nil
		}
	}, func(workspaces.Workspace) (string, error) {
		return "repository discovery incomplete", nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunSelectedWrapChildPassesNameAndCanonicalRoot(t *testing.T) {
	dir := t.TempDir()
	result := filepath.Join(dir, "result")
	t.Setenv("WRAP_CHILD_RESULT", result)
	script := filepath.Join(dir, "wrap-child")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\nprintf '%s\\n%s\\n%s' \"$1\" \"$2\" \"$3\" > \"$WRAP_CHILD_RESULT\"\n",
	), 0o755); err != nil {
		t.Fatal(err)
	}

	selected := workspaces.Workspace{Name: "alias", Root: "/real/service"}
	note, err := runSelectedWrapChild(script, selected)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("child note = %q, want empty", note)
	}
	got, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "tui-attach\nalias\n/real/service" {
		t.Fatalf("child arguments = %q", got)
	}
}

func TestRunSelectedWrapChildReturnsDetailedChildFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "wrap-child")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\nprintf '%s\\n' 'wrap: workspace \"alias\" is no longer active' >&2\nexit 1\n",
	), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := runSelectedWrapChild(script, workspaces.Workspace{
		Name: "alias", Root: "/real/service",
	})
	if err == nil || !strings.Contains(err.Error(), `workspace "alias" is no longer active`) {
		t.Fatalf("child failure = %v, want actionable stderr", err)
	}
}

func TestRunSelectedWrapChildDoesNotForwardUnsafeStderr(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "wrap-child")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\nprintf '\\033]2;unsafe\\007' >&2\nexit 1\n",
	), 0o755); err != nil {
		t.Fatal(err)
	}
	readStderr, writeStderr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStderr := os.Stderr
	os.Stderr = writeStderr
	t.Cleanup(func() {
		os.Stderr = originalStderr
		_ = readStderr.Close()
		_ = writeStderr.Close()
	})

	_, _ = runSelectedWrapChild(script, workspaces.Workspace{
		Name: "alias", Root: "/real/service",
	})
	if err := writeStderr.Close(); err != nil {
		t.Fatal(err)
	}
	forwarded, err := io.ReadAll(readStderr)
	if err != nil {
		t.Fatal(err)
	}
	if len(forwarded) != 0 {
		t.Fatalf("unsafe child stderr was forwarded before UI escaping: %q", forwarded)
	}
}

func TestRunSelectedWrapChildReturnsSuccessfulWarningsAsNote(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "wrap-child")
	if err := os.WriteFile(script, []byte(
		"#!/bin/sh\nprintf '%s\\n' 'wrap: repository discovery incomplete' >&2\n",
	), 0o755); err != nil {
		t.Fatal(err)
	}

	note, err := runSelectedWrapChild(script, workspaces.Workspace{
		Name: "alias", Root: "/real/service",
	})
	if err != nil {
		t.Fatal(err)
	}
	if note != "wrap: repository discovery incomplete" {
		t.Fatalf("child note = %q", note)
	}
}

func TestResolveSelectedWorkspacePreservesAliasIdentity(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ws, _, err := resolveSelectedWorkspace(
		workspaces.Workspace{Name: "alias", Root: root},
		nil,
		func(got string) ([]gitx.Discovered, []string, error) {
			if got != root {
				t.Fatalf("discover root = %q, want %q", got, root)
			}
			return nil, nil, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if ws.Name != "alias" || ws.Root != root {
		t.Fatalf("resolved selected workspace = %+v", ws)
	}
}

func TestResolveSelectedWorkspaceRejectsRetargetedCanonicalRoot(t *testing.T) {
	parent := t.TempDir()
	savedRoot := filepath.Join(parent, "service")
	if err := os.Mkdir(savedRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	originalRoot := filepath.Join(parent, "service-original")
	if err := os.Rename(savedRoot, originalRoot); err != nil {
		t.Fatal(err)
	}
	replacement := t.TempDir()
	if err := os.Symlink(replacement, savedRoot); err != nil {
		t.Fatal(err)
	}

	_, _, err := resolveSelectedWorkspace(
		workspaces.Workspace{Name: "service", Root: savedRoot},
		nil,
		func(string) ([]gitx.Discovered, []string, error) {
			return nil, nil, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "changed since selector refresh") {
		t.Fatalf("retargeted root error = %v", err)
	}
}

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
