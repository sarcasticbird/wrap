package config

import (
	"path/filepath"
	"runtime"
	"testing"
)

func TestExampleConfigLoads(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "examples", "wrap.toml")
	cfg, warns, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("warnings = %v", warns)
	}
	if cfg.WalkDepth != 1 || cfg.TreeSide != "left" || cfg.TreeWidth != 25 {
		t.Fatalf("layout = depth %d side %q width %d", cfg.WalkDepth, cfg.TreeSide, cfg.TreeWidth)
	}
	if cfg.Defaults.Cmd != "" {
		t.Fatalf("defaults.cmd = %q, want empty login shell", cfg.Defaults.Cmd)
	}
	want := Keys{FocusTree: "M-1", FocusTerminal: "M-2", FocusTerms: "M-3"}
	if cfg.Keys != want {
		t.Fatalf("keys = %+v, want %+v", cfg.Keys, want)
	}
}
