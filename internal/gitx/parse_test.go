package gitx

import "testing"

const porcelainFixture = `# branch.oid 4ae2299f6ac2c8b0c1a640f4dcb96d7e58ef9741
# branch.head main
# branch.upstream origin/main
# branch.ab +2 -1
1 .M N... 100644 100644 100644 aaa bbb internal/mod.go
1 M. N... 100644 100644 100644 aaa bbb staged only.go
1 MM N... 100644 100644 100644 aaa bbb both.go
2 R. N... 100644 100644 100644 aaa bbb R100 new.go	old.go
u UU N... 100644 100644 100644 100644 aaa bbb ccc conflict.go
? untracked.txt
? dir/another.txt
`

func TestParsePorcelainV2(t *testing.T) {
	s := parsePorcelainV2(porcelainFixture)
	if s.Branch != "main" || s.Upstream != "origin/main" {
		t.Errorf("branch %q upstream %q", s.Branch, s.Upstream)
	}
	if s.Ahead != 2 || s.Behind != 1 {
		t.Errorf("ahead/behind = %d/%d, want 2/1", s.Ahead, s.Behind)
	}
	wantStaged := []string{"staged only.go", "both.go", "new.go"}
	if len(s.Staged) != len(wantStaged) {
		t.Fatalf("staged = %+v", s.Staged)
	}
	for i, w := range wantStaged {
		if s.Staged[i].Path != w {
			t.Errorf("staged[%d] = %q, want %q", i, s.Staged[i].Path, w)
		}
	}
	// unstaged: mod.go (.M), both.go (MM), conflict.go (u)
	if len(s.Unstaged) != 3 || s.Unstaged[0].Path != "internal/mod.go" || s.Unstaged[2].Status != 'U' {
		t.Errorf("unstaged = %+v", s.Unstaged)
	}
	if len(s.Untracked) != 2 || s.Untracked[1] != "dir/another.txt" {
		t.Errorf("untracked = %+v", s.Untracked)
	}
	if s.Staged[2].Status != 'R' {
		t.Errorf("rename status = %c", s.Staged[2].Status)
	}
}

func TestParsePorcelainV2RejectsPartialAheadBehind(t *testing.T) {
	s := parsePorcelainV2("# branch.ab +7 -invalid\n")
	if s.Ahead != 0 || s.Behind != 0 {
		t.Errorf("malformed ahead/behind partially accepted as %d/%d", s.Ahead, s.Behind)
	}
}

func TestParseNumstat(t *testing.T) {
	out := "10\t2\tinternal/mod.go\x00" +
		"-\t-\timage.png\x00" +
		"3\t0\t\x00dir/old/f.go\x00dir/new/f.go\x00" +
		"5\t1\t\x00root-old.go\x00root-new.go\x00" +
		"7\t4\tbefore => after.txt\x00"
	m := parseNumstat(out)
	if m["internal/mod.go"] != [2]int{10, 2} {
		t.Errorf("mod.go = %v", m["internal/mod.go"])
	}
	if m["image.png"] != [2]int{0, 0} {
		t.Errorf("binary = %v", m["image.png"])
	}
	if m["dir/new/f.go"] != [2]int{3, 0} {
		t.Errorf("brace rename = %v; map: %v", m["dir/new/f.go"], m)
	}
	if m["root-new.go"] != [2]int{5, 1} {
		t.Errorf("bare rename = %v", m["root-new.go"])
	}
	if m["before => after.txt"] != [2]int{7, 4} {
		t.Errorf("literal arrow filename = %v; map: %v", m["before => after.txt"], m)
	}
}

func TestApplyCounts(t *testing.T) {
	files := []FileChange{{Path: "a.go"}, {Path: "b.go"}}
	applyCounts(files, map[string][2]int{"a.go": {7, 3}})
	if files[0].Added != 7 || files[0].Deleted != 3 {
		t.Errorf("a.go = %+v", files[0])
	}
	if files[1].Added != 0 {
		t.Errorf("b.go should be untouched: %+v", files[1])
	}
}

func TestUnquotePath(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain path untouched", "internal/foo.go", "internal/foo.go"},
		{"raw utf8 untouched (quotePath=false)", "café.txt", "café.txt"},
		{"space needs no quoting", "has space.txt", "has space.txt"},
		{"octal escapes rebuild utf8 bytewise", `"caf\303\251.txt"`, "café.txt"},
		{"cjk", `"\345\257\277\345\217\270.txt"`, "寿司.txt"},
		{"escaped quote", `"has\"quote.txt"`, `has"quote.txt`},
		{"escaped backslash", `"back\\slash.txt"`, `back\slash.txt`},
		{"newline in name", `"two\nlines.txt"`, "two\nlines.txt"},
		{"tab in name", `"a\tb.txt"`, "a\tb.txt"},
		// Malformed input is shown verbatim rather than mangled.
		{"unterminated quote", `"unterminated`, `"unterminated`},
		{"trailing backslash", `"bad\"`, `"bad\"`},
		{"unknown escape", `"bad\q.txt"`, `"bad\q.txt"`},
		{"short octal", `"bad\30"`, `"bad\30"`},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unquotePath(tc.in); got != tc.want {
				t.Errorf("unquotePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// An accented untracked file must reach the model as its real name, so the
// path handed back to `git diff` matches something.
func TestParsePorcelainDecodesQuotedPaths(t *testing.T) {
	out := "# branch.head main\n" +
		"? \"caf\\303\\251.txt\"\n" +
		"1 .M N... 100644 100644 100644 aaa bbb plain.txt\n"
	s := parsePorcelainV2(out)
	if len(s.Untracked) != 1 || s.Untracked[0] != "café.txt" {
		t.Errorf("untracked = %q, want [café.txt]", s.Untracked)
	}
	if len(s.Unstaged) != 1 || s.Unstaged[0].Path != "plain.txt" {
		t.Errorf("unstaged = %+v", s.Unstaged)
	}
}
