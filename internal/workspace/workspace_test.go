package workspace

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sarcasticbird/wrap/internal/config"
	"github.com/sarcasticbird/wrap/internal/gitx"
	"github.com/sarcasticbird/wrap/internal/state"
)

func fakeDiscover(found []gitx.Discovered, warns []string) func(string) ([]gitx.Discovered, []string, error) {
	return func(string) ([]gitx.Discovered, []string, error) { return found, warns, nil }
}

func noDiscover(string) ([]gitx.Discovered, []string, error) { return nil, nil, nil }

func testCfg() *config.Config {
	return &config.Config{Keys: config.Keys{FocusTree: "M-a"}}
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestResolveEmptyTargetUsesCwd(t *testing.T) {
	dir := t.TempDir()
	found := []gitx.Discovered{
		{Name: "container/main", Path: filepath.Join(dir, "container", "main"), Kind: gitx.DiscoveredWorktree},
		{Name: "plain", Path: filepath.Join(dir, "plain"), Kind: gitx.DiscoveredRepo},
	}
	w, warns, err := Resolve("", dir, nil, fakeDiscover(found, []string{"note"}))
	if err != nil {
		t.Fatal(err)
	}
	if w.Root != canonicalPath(t, dir) {
		t.Fatalf("w = %+v", w)
	}
	if w.Name != config.SanitizeName(filepath.Base(dir)) {
		t.Errorf("name = %q", w.Name)
	}
	if len(warns) != 1 || warns[0] != "note" {
		t.Errorf("warns = %v", warns)
	}
	if len(w.Repos) != 2 || w.Repos[0].Name != "container/main" || w.Repos[1].Name != "plain" {
		t.Errorf("repos = %+v", w.Repos)
	}
	// No config → sessions run the login shell; wrap invents no command.
	if w.Cmd != "" {
		t.Errorf("Cmd = %q, want empty without a config", w.Cmd)
	}
	// A TOML [defaults] cmd (and keys) are inherited by folder workspaces verbatim.
	cfg := testCfg()
	cfg.Defaults.Cmd = "claude"
	w2, _, err := Resolve("", dir, cfg, fakeDiscover(found, nil))
	if err != nil {
		t.Fatal(err)
	}
	if w2.Cmd != "claude" {
		t.Errorf("Cmd = %q, want claude from TOML", w2.Cmd)
	}
	if w2.Keys != cfg.Keys {
		t.Errorf("keys not carried: %+v", w2.Keys)
	}
}

func TestResolveEmptyFolderIsValid(t *testing.T) {
	w, _, err := Resolve("", t.TempDir(), nil, noDiscover)
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Repos) != 0 {
		t.Errorf("Repos = %+v, want empty (a repo-less folder is a valid workspace)", w.Repos)
	}
}

func TestResolveExplicitDir(t *testing.T) {
	dir := t.TempDir()
	w, _, err := Resolve(dir, "/elsewhere", nil, noDiscover)
	if err != nil {
		t.Fatal(err)
	}
	if w.Root != canonicalPath(t, dir) {
		t.Errorf("w = %+v", w)
	}
	if _, _, err := Resolve(filepath.Join(dir, "missing"), "/x", nil, noDiscover); err == nil {
		t.Error("nonexistent dir should error")
	}
}

func TestResolveFilesystemRootUsesStableName(t *testing.T) {
	for _, tc := range []struct {
		name, target, cwd string
	}{
		{name: "explicit", target: "/", cwd: t.TempDir()},
		{name: "current directory", target: "", cwd: "/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, _, err := Resolve(tc.target, tc.cwd, nil, noDiscover)
			if err != nil {
				t.Fatal(err)
			}
			if w.Root != "/" || w.Name != "root" {
				t.Fatalf("Resolve(root) = %+v, want Root=/ Name=root", w)
			}
		})
	}
}

func TestResolveRelativeTargetUsesProvidedCwd(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "project")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	w, _, err := Resolve("project", parent, nil, noDiscover)
	if err != nil {
		t.Fatal(err)
	}
	if w.Root != canonicalPath(t, root) {
		t.Fatalf("Root = %q, want %q", w.Root, canonicalPath(t, root))
	}
}

func TestResolveCanonicalizesWorkspaceRoot(t *testing.T) {
	realRoot := t.TempDir()
	wantRoot := canonicalPath(t, realRoot)
	parent := t.TempDir()
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	discover := func(got string) ([]gitx.Discovered, []string, error) {
		if got != wantRoot {
			t.Errorf("discover root = %q, want %q", got, wantRoot)
		}
		return nil, nil, nil
	}
	w, _, err := Resolve(alias, parent, nil, discover)
	if err != nil {
		t.Fatal(err)
	}
	if w.Root != wantRoot {
		t.Fatalf("Root = %q, want %q", w.Root, wantRoot)
	}
	if w.Name != "alias" {
		t.Fatalf("Name = %q, want lexical alias name preserved", w.Name)
	}
}

func TestResolveRejectsReservedWorkspaceName(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, config.HomeSession)
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := Resolve(root, parent, nil, noDiscover)
	if err == nil || !strings.Contains(err.Error(), "resolve workspace") || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("err = %v, want contextual reserved-name error", err)
	}
}

func TestFromMetaWrapsWorkspaceValidationError(t *testing.T) {
	_, _, err := FromMeta("bad\nname", state.Meta{Kind: "folder", Root: "/root"}, nil, noDiscover)
	if err == nil || !strings.Contains(err.Error(), "reconstruct workspace") {
		t.Fatalf("err = %v, want reconstruction context", err)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	found := []gitx.Discovered{{Name: "r", Path: filepath.Join(dir, "r"), Kind: gitx.DiscoveredRepo}}
	w, _, err := Resolve(dir, "/x", nil, fakeDiscover(found, nil))
	if err != nil {
		t.Fatal(err)
	}
	m := w.Meta()
	wantRoot := canonicalPath(t, dir)
	if m.Kind != "folder" || m.Root != wantRoot {
		t.Errorf("meta = %+v", m)
	}
	w2, _, err := FromMeta(w.Name, m, nil, fakeDiscover(found, nil))
	if err != nil {
		t.Fatal(err)
	}
	if w2.Root != wantRoot || w2.Name != w.Name {
		t.Errorf("w2 = %+v", w2)
	}
}

func TestResolveSurfacesDiscoverError(t *testing.T) {
	failing := func(string) ([]gitx.Discovered, []string, error) {
		return nil, nil, fmt.Errorf("permission denied")
	}
	// Empty target (cwd) and an explicit dir both surface the real
	// discover failure directly — there is no fallback workspace.
	if _, _, err := Resolve("", t.TempDir(), testCfg(), failing); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the discover failure", err)
	}
	if _, _, err := Resolve(t.TempDir(), "/x", nil, failing); err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("err = %v, want the discover failure", err)
	}
}

func TestResolveExplicitDirIsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Resolve(f, "/x", nil, noDiscover)
	if err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("err = %v, want not-a-directory", err)
	}
}

func TestResolveExplicitDirStatErrorSurfaces(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	_, _, err := Resolve(missing, "/x", nil, noDiscover)
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("err = %v, want the underlying stat error surfaced", err)
	}
}

func TestResolveCarriesTreeSide(t *testing.T) {
	dir := t.TempDir()
	w, _, err := Resolve("", dir, nil, noDiscover)
	if err != nil {
		t.Fatal(err)
	}
	if w.TreeSide != "left" {
		t.Errorf("TreeSide = %q, want left (default, nil cfg)", w.TreeSide)
	}

	cfg := testCfg()
	cfg.TreeSide = "right"
	w2, _, err := Resolve("", dir, cfg, noDiscover)
	if err != nil {
		t.Fatal(err)
	}
	if w2.TreeSide != "right" {
		t.Errorf("TreeSide = %q, want right (carried from cfg)", w2.TreeSide)
	}
}

func TestResolveCarriesTreeWidth(t *testing.T) {
	dir := t.TempDir()
	w, _, err := Resolve("", dir, nil, noDiscover)
	if err != nil {
		t.Fatal(err)
	}
	if w.TreeWidth != 25 {
		t.Errorf("TreeWidth = %d, want 25 (default, nil cfg)", w.TreeWidth)
	}

	cfg := testCfg()
	cfg.TreeWidth = 40
	w2, _, err := Resolve("", dir, cfg, noDiscover)
	if err != nil {
		t.Fatal(err)
	}
	if w2.TreeWidth != 40 {
		t.Errorf("TreeWidth = %d, want 40 (carried from cfg)", w2.TreeWidth)
	}
}

func TestResolveCarriesWalkDepth(t *testing.T) {
	dir := t.TempDir()
	w, _, err := Resolve("", dir, nil, noDiscover)
	if err != nil {
		t.Fatal(err)
	}
	if w.WalkDepth != 1 {
		t.Errorf("WalkDepth = %d, want 1 (default, nil cfg)", w.WalkDepth)
	}

	cfg := testCfg()
	cfg.WalkDepth = 3
	w2, _, err := Resolve("", dir, cfg, noDiscover)
	if err != nil {
		t.Fatal(err)
	}
	if w2.WalkDepth != 3 {
		t.Errorf("WalkDepth = %d, want 3 (carried from cfg)", w2.WalkDepth)
	}
}

func TestInitialSelection(t *testing.T) {
	dir := t.TempDir()
	found := []gitx.Discovered{{Name: "r", Path: filepath.Join(dir, "r"), Kind: gitx.DiscoveredRepo}}
	w, _, err := Resolve(dir, "/x", nil, fakeDiscover(found, nil))
	if err != nil {
		t.Fatal(err)
	}
	sel, cmd := w.InitialSelection()
	if sel.Session != w.Name || sel.Path != canonicalPath(t, dir) {
		t.Errorf("sel = %+v (ws name %q)", sel, w.Name)
	}
	if cmd != "" {
		t.Errorf("cmd = %q, want empty (login shell) without a config", cmd)
	}
}
