package pane

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"
	"github.com/sarcasticbird/wrap/internal/config"
)

func TestFocusLabel(t *testing.T) {
	tests := map[string]string{
		"M-1": "⌥1",
		"M-a": "⌥a",
		"C-a": "C-a",
	}
	for in, want := range tests {
		if got := FocusLabel(in); got != want {
			t.Errorf("FocusLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPaneChromeUsesEffectiveKeys(t *testing.T) {
	keys := config.Keys{
		FocusTerminal: "M-a",
		FocusTree:     "C-g",
		FocusTerms:    "M-9",
	}
	if got := ansi.Strip(Heading("Git", keys.FocusTree, 40)); got != "Git (C-g)" {
		t.Fatalf("heading = %q", got)
	}
	help := ansi.Strip(HelpBody(keys, 80, 20))
	for _, want := range []string{"⌥a terminal", "C-g Git", "⌥9 Terminals", "m mirror"} {
		if !strings.Contains(help, want) {
			t.Errorf("HelpBody missing %q:\n%s", want, help)
		}
	}
}

func TestPaneChromeClipsByDisplayWidthAndHeight(t *testing.T) {
	heading := ansi.Strip(Heading("Terminals", "M-3", 8))
	if got := runewidth.StringWidth(heading); got > 8 {
		t.Fatalf("heading width = %d: %q", got, heading)
	}
	lines := strings.Split(ansi.Strip(HelpBody(config.Keys{}.WithDefaults(), 12, 3)), "\n")
	if len(lines) > 3 {
		t.Fatalf("help lines = %d, want <= 3", len(lines))
	}
	for _, line := range lines {
		if got := runewidth.StringWidth(line); got > 12 {
			t.Fatalf("line width = %d: %q", got, line)
		}
	}
}

func TestHelpBodyDistinguishesNoSpaceFromUnknownHeight(t *testing.T) {
	keys := config.Keys{}.WithDefaults()
	if got := HelpBody(keys, 80, 0); got != "" {
		t.Fatalf("HelpBody with no available rows = %q, want empty", ansi.Strip(got))
	}
	if got := len(strings.Split(ansi.Strip(HelpBody(keys, 80, -1)), "\n")); got != 6 {
		t.Fatalf("HelpBody with unknown height has %d lines, want 6", got)
	}
}

func TestPaneChromeFooters(t *testing.T) {
	if got := ansi.Strip(ActionFooter(80)); got != "h help · q detach · Q shutdown" {
		t.Fatalf("ActionFooter = %q", got)
	}
	if got := ansi.Strip(HelpFooter(80)); got != "h / esc / q close" {
		t.Fatalf("HelpFooter = %q", got)
	}
}
