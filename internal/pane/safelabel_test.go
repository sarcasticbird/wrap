package pane

import (
	"strings"
	"testing"
)

// TestSafeLabelNeutralizesEscapeBytes proves the helper strips the raw
// control bytes a hostile repo can smuggle through a filename or branch
// name, so nothing an attacker names can reach the terminal as an escape
// sequence. This is the guard behind the OSC 52 clipboard-hijack the tree
// pane would otherwise forward.
func TestSafeLabelNeutralizesEscapeBytes(t *testing.T) {
	// A file literally named to inject OSC 52 (set clipboard) then a BEL
	// terminator, exactly what git C-quoting reconstructs into raw bytes.
	name := "ev\x1b]52;c;cm0gLXJmIH4=\ail.txt"
	got := SafeLabel(name)
	for _, r := range got {
		if r < 0x20 || r == 0x7f {
			t.Fatalf("SafeLabel left control byte %#x in output %q", r, got)
		}
	}
	if !strings.Contains(got, "il.txt") {
		t.Errorf("SafeLabel dropped legible text; got %q", got)
	}
}

// TestSafeLabelLeavesPrintableUnicodeAlone keeps the helper from mangling
// legitimate names: non-ASCII graphic runes must survive untouched so real
// repositories and branches still read correctly.
func TestSafeLabelLeavesPrintableUnicodeAlone(t *testing.T) {
	name := "café-λ-项目"
	if got := SafeLabel(name); got != name {
		t.Errorf("SafeLabel altered printable text: got %q, want %q", got, name)
	}
}
