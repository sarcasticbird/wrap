// Package gitx shells out to git and parses machine-readable output.
package gitx

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var ErrNotARepo = errors.New("not a git repository")

// HasMetadata reports whether dir or one of its ancestors contains worktree
// Git metadata. It follows Git's upward discovery shape and uses Lstat so a
// broken or inaccessible .git entry is still distinguished from an ordinary
// folder outside a repository.
func HasMetadata(dir string) (bool, error) {
	current, err := filepath.Abs(dir)
	if err != nil {
		return false, fmt.Errorf("inspect Git metadata in %s: %w", dir, err)
	}
	for {
		_, err := os.Lstat(filepath.Join(current, ".git"))
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, os.ErrNotExist):
			// Keep walking.
		default:
			return false, fmt.Errorf("inspect Git metadata in %s: %w", current, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
	}
}

// Status is Take without diff stats: branch, ahead/behind, and file
// lists only. Cheap enough to run per child in the parent overview.
func Status(dir string) (*Snapshot, error) {
	root, err := Toplevel(dir)
	if err != nil {
		return nil, err
	}
	sub, pathspec, err := scopePathspec(root, dir)
	if err != nil {
		return nil, err
	}
	out, err := run(root, append([]string{"status", "--porcelain=v2", "--branch", "-uall"}, pathspec...)...)
	if err != nil {
		return nil, err
	}
	s := parsePorcelainV2(out)
	s.RepoRoot = root
	s.Subdir = sub
	return s, nil
}

func run(dir string, args ...string) (string, error) {
	// core.quotePath=false stops git C-quoting non-ASCII paths. Left on
	// (the default), an accented or CJK filename comes back as
	// "caf\303\251.txt"; wrap stored that literal as the path, showed it
	// in the tree, and handed it straight back to `git diff -- <path>`,
	// which then matched nothing and opened an empty pager.
	//
	// git still quotes paths containing quotes, backslashes or control
	// characters, and that is load-bearing: a raw newline in a filename
	// would otherwise split one status record across two lines. Those
	// come back quoted and are decoded by unquotePath at parse time.
	// Disable the repository-configured execution points Git lets these
	// queries neutralize generically. Clean/process filter driver names are
	// attribute-selected and have no equivalent all-drivers-off switch, so
	// callers must still treat opened repositories as trusted inputs.
	prefix := []string{
		"-c", "core.quotePath=false",
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=" + os.DevNull,
	}
	cmd := exec.Command("git", append(prefix, args...)...)
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "LC_ALL=") || strings.HasPrefix(entry, "GIT_OPTIONAL_LOCKS=") {
			continue
		}
		env = append(env, entry)
	}
	cmd.Env = append(env, "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		stderr := strings.TrimSpace(errb.String())
		if strings.Contains(stderr, "not a git repository") {
			return "", ErrNotARepo
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, stderr)
	}
	return out.String(), nil
}

// Toplevel returns the repo root containing dir, or ErrNotARepo.
func Toplevel(dir string) (string, error) {
	out, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	out = strings.TrimSuffix(out, "\n")
	out = strings.TrimSuffix(out, "\r")
	return out, nil
}

// Take produces a full status snapshot for dir. When dir sits below the
// repo root, status and diff stats are scoped to that subtree.
func Take(dir string) (*Snapshot, error) {
	root, err := Toplevel(dir)
	if err != nil {
		return nil, err
	}
	sub, pathspec, err := scopePathspec(root, dir)
	if err != nil {
		return nil, err
	}
	out, err := run(root, append([]string{"status", "--porcelain=v2", "--branch", "-uall"}, pathspec...)...)
	if err != nil {
		return nil, err
	}
	s := parsePorcelainV2(out)
	s.RepoRoot, s.Subdir = root, sub
	un, err := run(root, append([]string{"diff", "--no-ext-diff", "--no-textconv", "--numstat", "-z"}, pathspec...)...)
	if err != nil {
		return nil, err
	}
	applyCounts(s.Unstaged, parseNumstat(un))
	st, err := run(root, append([]string{"diff", "--no-ext-diff", "--no-textconv", "--cached", "--numstat", "-z"}, pathspec...)...)
	if err != nil {
		return nil, err
	}
	applyCounts(s.Staged, parseNumstat(st))
	return s, nil
}

func scopePathspec(root, dir string) (string, []string, error) {
	// Resolve both sides so macOS /var vs /private/var symlinks agree. A
	// failed resolution must stop the query: omitting the pathspec would
	// silently return status and counts for the entire repository.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", nil, fmt.Errorf("make repository root absolute %s: %w", root, err)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", nil, fmt.Errorf("make requested directory absolute %s: %w", dir, err)
	}
	rroot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return "", nil, fmt.Errorf("resolve repository root %s: %w", root, err)
	}
	rdir, err := filepath.EvalSymlinks(absDir)
	if err != nil {
		return "", nil, fmt.Errorf("resolve requested directory %s: %w", dir, err)
	}
	rel, err := filepath.Rel(rroot, rdir)
	if err != nil {
		return "", nil, fmt.Errorf("locate requested directory %s under repository root %s: %w", rdir, rroot, err)
	}
	if rel == "." {
		return "", nil, nil
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", nil, fmt.Errorf("requested directory %s is outside repository root %s", rdir, rroot)
	}
	return rel, []string{"--", ":(literal)" + filepath.ToSlash(rel)}, nil
}

// Worktree is one entry from `git worktree list --porcelain`.
type Worktree struct {
	Path   string
	Branch string
}

// Worktrees lists all non-bare worktrees of the repo containing dir.
func Worktrees(dir string) ([]Worktree, error) {
	out, err := run(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	return parseWorktrees(out), nil
}

func parseWorktrees(out string) []Worktree {
	var wts []Worktree
	var cur Worktree
	bare, prunable := false, false
	flush := func() {
		if cur.Path != "" && !bare && !prunable {
			wts = append(wts, cur)
		}
		cur, bare, prunable = Worktree{}, false, false
	}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur.Path = unquotePath(strings.TrimPrefix(line, "worktree "))
		case strings.HasPrefix(line, "branch refs/heads/"):
			cur.Branch = strings.TrimPrefix(line, "branch refs/heads/")
		case strings.HasPrefix(line, "prunable"):
			prunable = true
		case line == "bare":
			bare = true
		case line == "detached":
			cur.Branch = "(detached)"
		}
	}
	flush()
	return wts
}

type DiscoveredKind int

const (
	// DiscoveredRepo is a plain repo (a .git dir or file).
	DiscoveredRepo DiscoveredKind = iota
	// DiscoveredWorktree is a worktree of a bare-repo container
	// (a subdir holding .bare plus named worktree dirs).
	DiscoveredWorktree
)

type Discovered struct {
	Name string // "repo" or "container/worktree"
	Path string
	Kind DiscoveredKind
}

// Discover scans up to depth levels below root for repos. Plain repos
// and bare-repo containers terminate recursion; plain directories are
// entered while depth remains. Names are /-joined relative paths.
// Per-child failures are warnings; only an unreadable root errors.
func Discover(root string, depth int) ([]Discovered, []string, error) {
	if depth < 1 {
		depth = 1
	}
	return discoverAt(root, "", depth)
}

// statMetadata distinguishes a genuinely absent metadata path from an existing
// symlink that cannot be resolved. os.Stat alone reports os.ErrNotExist for
// both, which would make a dangling .git/.bare link silently disappear from
// topology-completeness checks.
func statMetadata(path string) (os.FileInfo, bool, error) {
	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, true, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, true, err
	}
	return fi, true, nil
}

func discoverAt(dir, prefix string, depth int) ([]Discovered, []string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if prefix != "" {
			return nil, []string{fmt.Sprintf("%s: %v", prefix, err)}, nil
		}
		return nil, nil, fmt.Errorf("discover %s: %w", dir, err)
	}
	var found []Discovered
	var warns []string
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		name := e.Name()
		if prefix != "" {
			name = prefix + "/" + e.Name()
		}
		sub := filepath.Join(dir, e.Name())
		_, gitExists, gitErr := statMetadata(filepath.Join(sub, ".git"))
		if gitErr != nil {
			warns = append(warns, fmt.Sprintf("%s: inspect .git: %v", name, gitErr))
			continue
		}
		if gitExists {
			found = append(found, Discovered{Name: name, Path: sub, Kind: DiscoveredRepo})
			continue
		}
		bare := filepath.Join(sub, ".bare")
		fi, bareExists, bareErr := statMetadata(bare)
		if bareErr != nil {
			warns = append(warns, fmt.Sprintf("%s: inspect .bare: %v", name, bareErr))
			continue
		}
		if bareExists && fi.IsDir() {
			wts, werr := Worktrees(bare)
			if werr != nil {
				warns = append(warns, fmt.Sprintf("%s: %v", name, werr))
				continue
			}
			for _, w := range wts {
				found = append(found, Discovered{
					Name: name + "/" + filepath.Base(w.Path),
					Path: w.Path,
					Kind: DiscoveredWorktree,
				})
			}
			continue
		}
		if depth > 1 {
			deeper, dwarns, _ := discoverAt(sub, name, depth-1)
			found = append(found, deeper...)
			warns = append(warns, dwarns...)
		}
	}
	return found, warns, nil
}
