package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "wrap.toml")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSessionName(t *testing.T) {
	if got := SessionName("my.proj", "vb server:main"); got != "my_proj/vb~20server~3amain" {
		t.Errorf("SessionName = %q", got)
	}
}

func TestSessionNameDoesNotCollapseDistinctEntries(t *testing.T) {
	names := map[string]bool{}
	for _, entry := range []string{"foo.bar", "foo:bar", "foo bar", "foo_bar", "foo~2ebar"} {
		name := SessionName("ws", entry)
		if names[name] {
			t.Fatalf("SessionName collapsed distinct entry %q to %q", entry, name)
		}
		names[name] = true
	}
}

func TestLegacySessionNameDocumentsPreEncodingFormat(t *testing.T) {
	if got := LegacySessionName("my.proj", "vb server:main"); got != "my_proj/vb_server_main" {
		t.Fatalf("LegacySessionName = %q", got)
	}
}

func TestValidateWorkspaceName(t *testing.T) {
	for _, name := range []string{"project", "my_project", "équipe", "project;it's-safe", "~project"} {
		if err := ValidateWorkspaceName(name); err != nil {
			t.Errorf("ValidateWorkspaceName(%q): %v", name, err)
		}
	}
	for _, name := range []string{
		"", ".", "..", "a/b", `a\b`, HomeSession,
		"project·term", "project·term·1", "project·term·logs", "project·diff",
		"project$USER", "line\nbreak", "tab\tname", "escape\x1bname", string([]byte{0xff}),
	} {
		if err := ValidateWorkspaceName(name); err == nil {
			t.Errorf("ValidateWorkspaceName(%q) = nil, want error", name)
		}
	}
}

func TestWorkspaceNameCannotOverlapAnotherTermNamespace(t *testing.T) {
	const owner = "project"
	collider := owner + "·term"
	if generated := TermPrefix(collider) + "1"; !SessionOwnedBy(owner, generated) {
		t.Fatalf("test premise failed: %q should overlap %q's term namespace", generated, owner)
	}
	if err := ValidateWorkspaceName(collider); err == nil {
		t.Fatalf("ValidateWorkspaceName(%q) = nil, want namespace collision error", collider)
	}
}

func TestSessionOwnedBy(t *testing.T) {
	cases := []struct {
		desc, ws, name string
		want           bool
	}{
		{"root session", "vb", "vb", true},
		{"named entry", "vb", "vb/x", true},
		{"scratch terminal", "vb", "vb·term·1", true},
		{"renamed scratch terminal", "vb", "vb·term·logs", true},
		{"diff pager", "vb", "vb·diff", true},
		{"other workspace root", "vb", "other", false},
		{"other workspace entry", "vb", "other/y", false},
		{"other workspace diff pager", "vb", "other·diff", false},
		{"home session", "vb", "wrap-home", false},
		{"near-miss prefix without delimiter", "vb", "vbextra", false},
		{"diff-like suffix without dot delimiter", "vb", "vb·diffx", false},
		{"term-like prefix without namespace sep", "vb", "vbx·diff", false},
	}
	for _, c := range cases {
		if got := SessionOwnedBy(c.ws, c.name); got != c.want {
			t.Errorf("%s: SessionOwnedBy(%q, %q) = %v, want %v", c.desc, c.ws, c.name, got, c.want)
		}
	}
}

func TestWalkDepth(t *testing.T) {
	c, _, err := Load(writeConfig(t, "walk_depth = 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.WalkDepth != 3 {
		t.Errorf("WalkDepth = %d", c.WalkDepth)
	}
	c, _, err = Load(writeConfig(t, "[defaults]\ncmd = \"x\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.WalkDepth != 1 {
		t.Errorf("default WalkDepth = %d, want 1", c.WalkDepth)
	}
	c, _, err = Load(writeConfig(t, "walk_depth = 99\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.WalkDepth != 5 {
		t.Errorf("capped WalkDepth = %d, want 5", c.WalkDepth)
	}
}

func TestWalkDepthInsideDefaults(t *testing.T) {
	c, _, err := Load(writeConfig(t, "[defaults]\ncmd = \"\"\n\nwalk_depth = 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.WalkDepth != 3 {
		t.Errorf("WalkDepth = %d, want 3 (defaults-table position must work)", c.WalkDepth)
	}
}

func TestKeysWithDefaults(t *testing.T) {
	want := Keys{FocusTree: "M-2", FocusTerminal: "M-1", FocusTerms: "M-3"}
	if got := (Keys{}).WithDefaults(); got != want {
		t.Fatalf("WithDefaults() = %+v, want %+v", got, want)
	}
	got := (Keys{FocusTree: "M-a"}).WithDefaults()
	if got.FocusTree != "M-a" || got.FocusTerminal != "M-1" || got.FocusTerms != "M-3" {
		t.Fatalf("partial defaults = %+v", got)
	}

	tests := map[string]struct {
		keys Keys
		want Keys
	}{
		"legacy tree binding": {
			keys: Keys{FocusTree: "M-1"},
			want: Keys{FocusTree: "M-1", FocusTerminal: "M-2", FocusTerms: "M-3"},
		},
		"legacy terminal binding": {
			keys: Keys{FocusTerminal: "M-2"},
			want: Keys{FocusTree: "M-1", FocusTerminal: "M-2", FocusTerms: "M-3"},
		},
		"default key claimed by terms": {
			keys: Keys{FocusTerms: "M-1"},
			want: Keys{FocusTree: "M-2", FocusTerminal: "M-3", FocusTerms: "M-1"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := tt.keys.WithDefaults(); got != tt.want {
				t.Fatalf("WithDefaults() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestDefaultPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if p, _ := DefaultPath(); p != "/xdg/wrap/wrap.toml" {
		t.Errorf("with XDG: %q", p)
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/h")
	if p, _ := DefaultPath(); p != "/h/.config/wrap/wrap.toml" {
		t.Errorf("without XDG: %q", p)
	}
}

func TestTreeSideDefault(t *testing.T) {
	c, _, err := Load(writeConfig(t, "walk_depth = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TreeSide != "left" {
		t.Errorf("TreeSide = %q, want left", c.TreeSide)
	}
}

func TestTreeSideExplicitRight(t *testing.T) {
	c, _, err := Load(writeConfig(t, "tree_side = \"right\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TreeSide != "right" {
		t.Errorf("TreeSide = %q, want right", c.TreeSide)
	}
}

func TestTreeSideInvalidRejected(t *testing.T) {
	_, _, err := Load(writeConfig(t, "tree_side = \"up\"\n"))
	if err == nil {
		t.Fatal("expected an error for an invalid tree_side")
	}
	if !strings.Contains(err.Error(), "up") {
		t.Errorf("error should name the bad value, got: %v", err)
	}
}

func TestTreeWidthDefault(t *testing.T) {
	c, _, err := Load(writeConfig(t, "walk_depth = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TreeWidth != 25 {
		t.Errorf("TreeWidth = %d, want 25", c.TreeWidth)
	}
}

func TestTreeWidthExplicit(t *testing.T) {
	c, _, err := Load(writeConfig(t, "tree_width = 40\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TreeWidth != 40 {
		t.Errorf("TreeWidth = %d, want 40", c.TreeWidth)
	}
}

func TestTreeWidthClampMin(t *testing.T) {
	c, _, err := Load(writeConfig(t, "tree_width = 3\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TreeWidth != 10 {
		t.Errorf("TreeWidth = %d, want clamped to 10", c.TreeWidth)
	}
}

func TestTreeWidthClampMax(t *testing.T) {
	c, _, err := Load(writeConfig(t, "tree_width = 90\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TreeWidth != 60 {
		t.Errorf("TreeWidth = %d, want clamped to 60", c.TreeWidth)
	}
}

func TestLoadDefaults(t *testing.T) {
	c, _, err := Load(writeConfig(t, "[defaults]\ncmd = \"myagent\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Defaults.Cmd != "myagent" {
		t.Errorf("Defaults.Cmd = %q, want myagent", c.Defaults.Cmd)
	}
}

func TestKnobsInsideDefaultsTable(t *testing.T) {
	// The exact file shape that silently no-opped in the wild: knobs
	// placed under [defaults]. Both must parse from there.
	c, warns, err := Load(writeConfig(t, "[defaults]\ncmd = \"\"\ntree_side = \"right\"\ntree_width = 15\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TreeSide != "right" || c.TreeWidth != 15 {
		t.Errorf("side=%q width=%d, want right/15 from defaults table", c.TreeSide, c.TreeWidth)
	}
	if len(warns) != 0 {
		t.Errorf("recognized keys must not warn: %v", warns)
	}
	// Top level wins when both positions are set.
	c, _, err = Load(writeConfig(t, "tree_side = \"left\"\n[defaults]\ntree_side = \"right\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TreeSide != "left" {
		t.Errorf("top level should win, got %q", c.TreeSide)
	}
}

func TestExplicitTopLevelZeroWinsNestedDefaults(t *testing.T) {
	c, _, err := Load(writeConfig(t, "walk_depth = 0\ntree_width = 0\n[defaults]\nwalk_depth = 3\ntree_width = 40\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.WalkDepth != 1 {
		t.Errorf("WalkDepth = %d, want explicit top-level zero clamped to 1", c.WalkDepth)
	}
	if c.TreeWidth != 10 {
		t.Errorf("TreeWidth = %d, want explicit top-level zero clamped to 10", c.TreeWidth)
	}
}

func TestExplicitNestedTreeWidthZeroMatchesTopLevel(t *testing.T) {
	c, _, err := Load(writeConfig(t, "[defaults]\ntree_width = 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.TreeWidth != 10 {
		t.Errorf("TreeWidth = %d, want explicit nested zero clamped to 10", c.TreeWidth)
	}
}

func TestUnknownKeysWarn(t *testing.T) {
	_, warns, err := Load(writeConfig(t, "tre_side = \"right\"\n[defaults]\nbogus = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 2 {
		t.Fatalf("warns = %v, want 2 (typo + unknown table key)", warns)
	}
	for _, w := range warns {
		if !strings.Contains(w, "unknown key") {
			t.Errorf("warning should name the problem: %q", w)
		}
	}
}
