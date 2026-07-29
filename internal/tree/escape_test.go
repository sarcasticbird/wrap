package tree

import "testing"

// hasControlByte reports whether s carries any C0 control byte or DEL — the
// bytes that let an escape sequence reach the outer terminal.
func hasControlByte(s string) (rune, bool) {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return r, true
		}
	}
	return 0, false
}

// TestRenderFileRowNeutralizesEscapeBytes is the regression guard for the
// one-keystroke OSC 52 clipboard hijack: a hostile repo ships a file whose
// name embeds raw escape bytes, the victim expands that repo in the tree,
// and the bytes must never reach the terminal.
func TestRenderFileRowNeutralizesEscapeBytes(t *testing.T) {
	var m Model
	r := row{kind: rowFile, untracked: true, name: "ev\x1b]52;c;cm0gLXJmIH4=\ail.txt"}
	if b, bad := hasControlByte(m.renderRow(r)); bad {
		t.Fatalf("rendered file row leaked control byte %#x to the terminal", b)
	}
}

// TestRenderRepoRowNeutralizesEscapeBytes covers the second source: the
// directory name and the git branch, both attacker-controlled for a cloned
// repository, are rendered on the repo row.
func TestRenderRepoRowNeutralizesEscapeBytes(t *testing.T) {
	m := Model{git: map[string]repoGit{
		"/evil": {branch: "main\x1b]0;pwned\a", dirty: 1},
	}}
	r := row{kind: rowRepo, name: "proj\x1b]52;c;x\a", path: "/evil", session: "ws/proj"}
	if b, bad := hasControlByte(m.renderRow(r)); bad {
		t.Fatalf("rendered repo row leaked control byte %#x to the terminal", b)
	}
}

// TestRenderRootRowNeutralizesEscapeBytes covers the workspace-root row,
// whose name comes from the post-symlink basename that bypasses the launch
// validation.
func TestRenderRootRowNeutralizesEscapeBytes(t *testing.T) {
	m := Model{git: map[string]repoGit{
		"/evil": {branch: "main\x1b]0;pwned\a", dirty: 1},
	}}
	r := row{kind: rowRoot, name: "root\x1b]52;c;x\a", path: "/evil", session: "ws"}
	if b, bad := hasControlByte(m.renderRow(r)); bad {
		t.Fatalf("rendered root row leaked control byte %#x to the terminal", b)
	}
}

// TestFooterNoteNeutralizesEscapeBytes covers the discovery-warning footer.
// Per-child discovery warnings embed the raw child directory name
// (gitx: "%s: inspect .git: %v"), so a hostile directory can smuggle an
// OSC 52 sequence into the note the tree renders at the bottom of the pane.
func TestFooterNoteNeutralizesEscapeBytes(t *testing.T) {
	m := Model{note: "proj\x1b]52;c;cm0gLXJmIH4=\a: inspect .git: not a directory"}
	if b, bad := hasControlByte(m.footer()); bad {
		t.Fatalf("footer note leaked control byte %#x to the terminal", b)
	}
}
