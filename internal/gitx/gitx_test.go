package gitx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// initRepo creates a repo with one committed file and returns its path.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	write(t, dir, "base.txt", "base\n")
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-m", "init")
	return dir
}

func TestToplevel(t *testing.T) {
	repo := initRepo(t)
	sub := filepath.Join(repo, "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// macOS TempDir is behind a /var → /private/var symlink; compare resolved paths.
	wantRoot, _ := filepath.EvalSymlinks(repo)
	got, err := Toplevel(sub)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := filepath.EvalSymlinks(got); resolved != wantRoot {
		t.Errorf("Toplevel = %q, want %q", got, wantRoot)
	}
	if _, err := Toplevel(t.TempDir()); !errors.Is(err, ErrNotARepo) {
		t.Errorf("non-repo error = %v, want ErrNotARepo", err)
	}
}

func TestToplevelPreservesTrailingWhitespace(t *testing.T) {
	parent := t.TempDir()
	repo := filepath.Join(parent, "repo ")
	git(t, parent, "init", "-b", "main", repo)
	want, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Toplevel(repo)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("Toplevel returned unusable path %q: %v", got, err)
	}
	if resolved != want {
		t.Fatalf("Toplevel = %q, want trailing whitespace preserved in %q", got, want)
	}
}

func TestTake(t *testing.T) {
	repo := initRepo(t)
	write(t, repo, "modified.txt", "one\ntwo\n")
	git(t, repo, "add", "modified.txt")
	git(t, repo, "commit", "-m", "add file")
	write(t, repo, "modified.txt", "one\ntwo\nthree\nfour\n") // +2 unstaged
	write(t, repo, "staged.txt", "s\n")
	git(t, repo, "add", "staged.txt")
	write(t, repo, "untracked.txt", "u\n")

	s, err := Take(repo)
	if err != nil {
		t.Fatal(err)
	}
	if s.Branch != "main" {
		t.Errorf("branch = %q", s.Branch)
	}
	if len(s.Staged) != 1 || s.Staged[0].Path != "staged.txt" || s.Staged[0].Added != 1 {
		t.Errorf("staged = %+v", s.Staged)
	}
	if len(s.Unstaged) != 1 || s.Unstaged[0].Path != "modified.txt" || s.Unstaged[0].Added != 2 {
		t.Errorf("unstaged = %+v", s.Unstaged)
	}
	if len(s.Untracked) != 1 || s.Untracked[0] != "untracked.txt" {
		t.Errorf("untracked = %+v", s.Untracked)
	}
}

func TestTakeSubdirScoping(t *testing.T) {
	repo := initRepo(t)
	write(t, repo, "apps/web/w.txt", "w\n")
	write(t, repo, "apps/api/a.txt", "a\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "apps")
	write(t, repo, "apps/web/w.txt", "changed\n")
	write(t, repo, "apps/api/a.txt", "changed\n")

	s, err := Take(filepath.Join(repo, "apps", "web"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Subdir != filepath.Join("apps", "web") {
		t.Errorf("subdir = %q", s.Subdir)
	}
	if len(s.Unstaged) != 1 || s.Unstaged[0].Path != "apps/web/w.txt" {
		t.Errorf("scoping failed: %+v", s.Unstaged)
	}
}

func TestStatusSubdirScoping(t *testing.T) {
	repo := initRepo(t)
	write(t, repo, "apps/web/w.txt", "w\n")
	write(t, repo, "apps/api/a.txt", "a\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "apps")
	write(t, repo, "apps/web/w.txt", "changed\n")
	write(t, repo, "apps/api/a.txt", "changed\n")

	s, err := Status(filepath.Join(repo, "apps", "web"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Subdir != filepath.Join("apps", "web") {
		t.Errorf("subdir = %q", s.Subdir)
	}
	if len(s.Unstaged) != 1 || s.Unstaged[0].Path != "apps/web/w.txt" {
		t.Errorf("status scoping failed: %+v", s.Unstaged)
	}
}

func TestTakeSubdirUsesLiteralPathspecAndAcceptsDotDotPrefix(t *testing.T) {
	repo := initRepo(t)
	write(t, repo, "..cache/inside.txt", "inside\n")
	write(t, repo, "glob*/inside.txt", "literal\n")
	write(t, repo, "glob-other/outside.txt", "outside\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "special directories")

	write(t, repo, "..cache/inside.txt", "changed\n")
	write(t, repo, "glob*/inside.txt", "changed\n")
	write(t, repo, "glob-other/outside.txt", "changed\n")

	dotdot, err := Take(filepath.Join(repo, "..cache"))
	if err != nil {
		t.Fatal(err)
	}
	if dotdot.Subdir != "..cache" || len(dotdot.Unstaged) != 1 ||
		dotdot.Unstaged[0].Path != "..cache/inside.txt" {
		t.Fatalf("dotdot-prefixed scope = %+v", dotdot)
	}

	literal, err := Take(filepath.Join(repo, "glob*"))
	if err != nil {
		t.Fatal(err)
	}
	if literal.Subdir != "glob*" || len(literal.Unstaged) != 1 ||
		literal.Unstaged[0].Path != "glob*/inside.txt" {
		t.Fatalf("glob-shaped scope = %+v", literal)
	}
}

func TestStatusOnly(t *testing.T) {
	repo := initRepo(t)
	write(t, repo, "u.txt", "u\n")
	s, err := Status(repo)
	if err != nil {
		t.Fatal(err)
	}
	if s.Branch != "main" {
		t.Errorf("branch = %q", s.Branch)
	}
	if len(s.Untracked) != 1 || s.Untracked[0] != "u.txt" {
		t.Errorf("untracked = %+v", s.Untracked)
	}
}

func TestStatusDisablesRepositoryFSMonitor(t *testing.T) {
	repo := initRepo(t)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor")
	t.Setenv("WRAP_TEST_FSMONITOR_MARKER", marker)
	write(t, filepath.Dir(hook), filepath.Base(hook), "#!/bin/sh\n: > \"$WRAP_TEST_FSMONITOR_MARKER\"\nprintf 'token\\0'\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "core.fsmonitor", hook)

	if _, err := Status(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository-configured fsmonitor executed during Status: stat err = %v", err)
	}
}

func TestStatusDoesNotWriteIndexOrRunHooks(t *testing.T) {
	repo := initRepo(t)
	marker := filepath.Join(t.TempDir(), "post-index-change-ran")
	hook := filepath.Join(repo, ".git", "hooks", "post-index-change")
	t.Setenv("WRAP_TEST_INDEX_HOOK_MARKER", marker)
	write(t, filepath.Dir(hook), filepath.Base(hook), "#!/bin/sh\n: > \"$WRAP_TEST_INDEX_HOOK_MARKER\"\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(repo, "base.txt"), future, future); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(repo, ".git", "index")
	before, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	if _, err := Status(repo); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(index)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("Status refreshed the repository index: before %s after %s", before.ModTime(), after.ModTime())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("post-index-change hook executed during Status: stat err = %v", err)
	}
}

func TestTakeDisablesRepositoryDiffPrograms(t *testing.T) {
	repo := initRepo(t)
	marker := filepath.Join(t.TempDir(), "diff-program-ran")
	program := filepath.Join(t.TempDir(), "diff-program")
	t.Setenv("WRAP_TEST_DIFF_MARKER", marker)
	write(t, filepath.Dir(program), filepath.Base(program), "#!/bin/sh\n: > \"$WRAP_TEST_DIFF_MARKER\"\n")
	if err := os.Chmod(program, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, repo, ".gitattributes", "*.txt diff=wrap-test\n")
	git(t, repo, "add", ".gitattributes")
	git(t, repo, "commit", "-m", "add attributes")
	git(t, repo, "config", "diff.wrap-test.command", program)
	git(t, repo, "config", "diff.wrap-test.textconv", program)
	write(t, repo, "base.txt", "changed\n")

	if _, err := Take(repo); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repository-configured diff program executed during Take: stat err = %v", err)
	}
}

func TestWorktrees(t *testing.T) {
	repo := initRepo(t)
	wt := filepath.Join(t.TempDir(), "feature-x")
	git(t, repo, "worktree", "add", "-b", "feature-x", wt)

	wts, err := Worktrees(repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(wts) != 2 {
		t.Fatalf("worktrees = %+v, want 2", wts)
	}
	var found bool
	for _, w := range wts {
		if w.Branch == "feature-x" {
			found = true
		}
	}
	if !found {
		t.Errorf("feature-x not listed: %+v", wts)
	}
}

func TestParseWorktreesSkipsPrunableAndUnquotesPaths(t *testing.T) {
	out := strings.Join([]string{
		`worktree /repo`,
		`branch refs/heads/main`,
		``,
		`worktree "/tmp/has\\backslash\"quote"`,
		`branch refs/heads/quoted`,
		``,
		`worktree /tmp/gone`,
		`branch refs/heads/stale`,
		`prunable gitdir file points to non-existent location`,
		``,
	}, "\n")
	got := parseWorktrees(out)
	if len(got) != 2 {
		t.Fatalf("parseWorktrees = %+v, want main and quoted only", got)
	}
	if got[1].Path != `/tmp/has\backslash"quote` || got[1].Branch != "quoted" {
		t.Fatalf("quoted worktree = %+v", got[1])
	}
}

func TestDiscoverWarnsWhenChildGitMetadataCannotBeInspected(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "broken")
	if err := os.Mkdir(child, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(child, ".git"), filepath.Join(child, ".git")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	found, warns, err := Discover(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("found = %v, want no repository", found)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "inspect .git") {
		t.Fatalf("warns = %v, want explicit .git inspection warning", warns)
	}
}

func TestDiscoverWarnsForDanglingMetadataSymlinks(t *testing.T) {
	for _, metadata := range []string{".git", ".bare"} {
		t.Run(metadata, func(t *testing.T) {
			root := t.TempDir()
			child := filepath.Join(root, "broken")
			if err := os.Mkdir(child, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(child, "missing-target"), filepath.Join(child, metadata)); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			found, warns, err := Discover(root, 1)
			if err != nil {
				t.Fatal(err)
			}
			if len(found) != 0 {
				t.Fatalf("found = %v, want no repository", found)
			}
			if len(warns) != 1 || !strings.Contains(warns[0], "inspect "+metadata) {
				t.Fatalf("warns = %v, want dangling %s warning", warns, metadata)
			}
		})
	}
}

func TestDiscover(t *testing.T) {
	root := t.TempDir()
	// Plain repo child.
	git(t, root, "init", "-b", "main", filepath.Join(root, "plain"))
	// Bare-container child: origin cloned bare into sub/.bare, one worktree.
	origin := initRepo(t)
	git(t, root, "clone", "--bare", origin, filepath.Join(root, "container", ".bare"))
	git(t, filepath.Join(root, "container", ".bare"), "worktree", "add", filepath.Join(root, "container", "main"))
	// Noise: plain dir and hidden dir are skipped.
	if err := os.MkdirAll(filepath.Join(root, "just-a-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".hidden"), 0o755); err != nil {
		t.Fatal(err)
	}

	found, warns, err := Discover(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Errorf("warns = %v", warns)
	}
	if len(found) != 2 {
		t.Fatalf("found = %+v, want plain repo + container worktree", found)
	}
	if found[0].Name != "container/main" || found[0].Kind != DiscoveredWorktree {
		t.Errorf("found[0] = %+v", found[0])
	}
	if found[1].Name != "plain" || found[1].Kind != DiscoveredRepo ||
		found[1].Path != filepath.Join(root, "plain") {
		t.Errorf("found[1] = %+v", found[1])
	}
}

func TestDiscoverWarnsOnBrokenContainer(t *testing.T) {
	root := t.TempDir()
	// A .bare that is not actually a git dir → warning, not fatal.
	if err := os.MkdirAll(filepath.Join(root, "broken", ".bare"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-b", "main", filepath.Join(root, "ok"))
	found, warns, err := Discover(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "ok" {
		t.Errorf("found = %+v", found)
	}
	if len(warns) != 1 {
		t.Errorf("warns = %v, want 1 for broken container", warns)
	}
}

func TestDiscoverDepth(t *testing.T) {
	root := t.TempDir()
	git(t, root, "init", "-b", "main", filepath.Join(root, "top"))
	if err := os.MkdirAll(filepath.Join(root, "group"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, root, "init", "-b", "main", filepath.Join(root, "group", "deep"))

	// Depth 1: plain dir "group" is opaque.
	found, _, err := Discover(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Name != "top" {
		t.Errorf("depth1 = %+v", found)
	}

	// Depth 2: nested repo appears with a path-joined name.
	found, _, err = Discover(root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 2 {
		t.Fatalf("depth2 = %+v", found)
	}
	if found[0].Name != "group/deep" || found[0].Kind != DiscoveredRepo {
		t.Errorf("nested = %+v", found[0])
	}
	if found[1].Name != "top" {
		t.Errorf("depth2 order = %+v", found)
	}

	// Depth 0 behaves like 1.
	found, _, err = Discover(root, 0)
	if err != nil || len(found) != 1 {
		t.Errorf("depth0 = %+v err=%v", found, err)
	}
}

// End-to-end cover for the common non-ASCII case: the name must survive
// into Snapshot intact AND still match its numstat counts. core.quotePath=false
// keeps both sides raw here, so this passes with or without unquoting —
// TestTakeCountsPathsGitStillQuotes is the one that pins the decode.
func TestTakeCountsNonASCIIPaths(t *testing.T) {
	repo := initRepo(t)
	const name = "café.txt"
	write(t, repo, name, "one\ntwo\nthree\n")
	git(t, repo, "add", "-A")

	s, err := Take(repo)
	if err != nil {
		t.Fatal(err)
	}
	var found *FileChange
	for i := range s.Staged {
		if s.Staged[i].Path == name {
			found = &s.Staged[i]
		}
	}
	if found == nil {
		paths := []string{}
		for _, f := range s.Staged {
			paths = append(paths, f.Path)
		}
		t.Fatalf("staged %q not found (paths: %v)", name, paths)
	}
	if found.Added != 3 {
		t.Errorf("Added = %d, want 3 — numstat key did not match the status path", found.Added)
	}
}

// git keeps C-quoting paths containing quotes, backslashes or control
// characters even with core.quotePath=false. Status paths are decoded, so
// the numstat keys must be decoded too or the +N/-M lookup misses and the
// file renders as "+0 -0".
func TestTakeCountsPathsGitStillQuotes(t *testing.T) {
	repo := initRepo(t)
	const name = `has"quote.txt`
	write(t, repo, name, "a\nb\nc\n")
	git(t, repo, "add", "-A")

	s, err := Take(repo)
	if err != nil {
		t.Fatal(err)
	}
	var found *FileChange
	for i := range s.Staged {
		if s.Staged[i].Path == name {
			found = &s.Staged[i]
		}
	}
	if found == nil {
		paths := []string{}
		for _, f := range s.Staged {
			paths = append(paths, f.Path)
		}
		t.Fatalf("staged %q not found (paths: %v)", name, paths)
	}
	if found.Added != 3 {
		t.Errorf("Added = %d, want 3 — numstat key did not match the decoded status path", found.Added)
	}
}

func TestTakeCountsLiteralRenameArrowFilename(t *testing.T) {
	repo := initRepo(t)
	const name = "before => after.txt"
	write(t, repo, name, "one\ntwo\nthree\n")
	git(t, repo, "add", "-A")

	s, err := Take(repo)
	if err != nil {
		t.Fatal(err)
	}
	for _, found := range s.Staged {
		if found.Path == name {
			if found.Added != 3 {
				t.Fatalf("Added = %d, want 3 for literal-arrow filename", found.Added)
			}
			return
		}
	}
	t.Fatalf("staged literal-arrow path not found: %+v", s.Staged)
}

func TestScopePathspecRejectsUnresolvableDirectory(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "gone")
	if _, _, err := scopePathspec(root, missing); err == nil {
		t.Fatal("unresolvable subtree silently became an unscoped repository query")
	} else if !strings.Contains(err.Error(), "resolve requested directory") {
		t.Fatalf("error = %q, want requested-directory context", err)
	}
}

func TestTakeAcceptsRelativeRepositoryAndSubdirectory(t *testing.T) {
	repo := initRepo(t)
	write(t, repo, "apps/web/app.go", "package web\n")
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "add app")
	write(t, repo, "base.txt", "changed\n")
	write(t, repo, "apps/web/app.go", "package changed\n")
	t.Chdir(repo)

	root, err := Take(".")
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Unstaged) != 2 {
		t.Fatalf("relative root returned %d changes, want 2: %+v", len(root.Unstaged), root.Unstaged)
	}

	sub, err := Take("apps/web")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Unstaged) != 1 || sub.Unstaged[0].Path != "apps/web/app.go" {
		t.Fatalf("relative subdirectory = %+v, want only apps/web/app.go", sub.Unstaged)
	}
}
