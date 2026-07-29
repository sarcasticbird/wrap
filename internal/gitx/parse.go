package gitx

import (
	"fmt"
	"strconv"
	"strings"
)

type FileChange struct {
	Path    string
	Status  byte // 'M','A','D','R','C','U', ...
	Added   int
	Deleted int
}

type Snapshot struct {
	RepoRoot  string
	Subdir    string // relative path when the entry is inside the repo, else ""
	Branch    string
	Upstream  string
	Ahead     int
	Behind    int
	Staged    []FileChange
	Unstaged  []FileChange
	Untracked []string
}

// parsePorcelainV2 parses `git status --porcelain=v2 --branch` output.
// Line formats: https://git-scm.com/docs/git-status#_porcelain_format_version_2
func parsePorcelainV2(out string) *Snapshot {
	s := &Snapshot{}
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "# branch.head "):
			s.Branch = strings.TrimPrefix(line, "# branch.head ")
		case strings.HasPrefix(line, "# branch.upstream "):
			s.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			var ahead, behind int
			if n, err := fmt.Sscanf(strings.TrimPrefix(line, "# branch.ab "), "+%d -%d", &ahead, &behind); err == nil && n == 2 {
				s.Ahead = ahead
				s.Behind = behind
			}
		case strings.HasPrefix(line, "1 "):
			f := strings.SplitN(line, " ", 9)
			if len(f) < 9 {
				continue
			}
			s.addXY(f[1], unquotePath(f[8]))
		case strings.HasPrefix(line, "2 "):
			f := strings.SplitN(line, " ", 10)
			if len(f) < 10 {
				continue
			}
			// f[9] is "<newPath>\t<origPath>"
			path, _, _ := strings.Cut(f[9], "\t")
			s.addXY(f[1], unquotePath(path))
		case strings.HasPrefix(line, "u "):
			f := strings.SplitN(line, " ", 11)
			if len(f) < 11 {
				continue
			}
			s.Unstaged = append(s.Unstaged, FileChange{Path: unquotePath(f[10]), Status: 'U'})
		case strings.HasPrefix(line, "? "):
			s.Untracked = append(s.Untracked, unquotePath(strings.TrimPrefix(line, "? ")))
		}
	}
	return s
}

// unquotePath decodes git's C-style quoting back to the real path.
//
// wrap runs git with core.quotePath=false, so ordinary non-ASCII names
// arrive raw. git still quotes a path containing a quote, a backslash or a
// control character — it has to, since a literal newline would otherwise
// split one status record across two lines — and those arrive wrapped in
// double quotes with C escapes inside. The path must be decoded before it
// is displayed or handed back to git as a pathspec.
//
// Octal escapes are decoded bytewise, not as runes: git emits one \nnn per
// BYTE of a multi-byte character, so \303\251 has to reassemble into the
// two bytes of "é" rather than two separate code points.
//
// Anything that is not a well-formed quoted string is returned unchanged;
// a path wrap cannot decode is better shown verbatim than mangled.
func unquotePath(p string) string {
	if len(p) < 2 || p[0] != '"' || p[len(p)-1] != '"' {
		return p
	}
	body := p[1 : len(p)-1]
	var b []byte
	for i := 0; i < len(body); {
		c := body[i]
		if c != '\\' {
			b = append(b, c)
			i++
			continue
		}
		if i+1 >= len(body) {
			return p // trailing backslash: not decodable
		}
		esc := body[i+1]
		switch esc {
		case 'a':
			b, i = append(b, '\a'), i+2
		case 'b':
			b, i = append(b, '\b'), i+2
		case 'f':
			b, i = append(b, '\f'), i+2
		case 'n':
			b, i = append(b, '\n'), i+2
		case 'r':
			b, i = append(b, '\r'), i+2
		case 't':
			b, i = append(b, '\t'), i+2
		case 'v':
			b, i = append(b, '\v'), i+2
		case '\\', '"':
			b, i = append(b, esc), i+2
		default:
			if esc < '0' || esc > '7' || i+3 >= len(body) {
				return p // unknown escape: leave the path alone
			}
			n, err := strconv.ParseUint(body[i+1:i+4], 8, 8)
			if err != nil {
				return p
			}
			b, i = append(b, byte(n)), i+4
		}
	}
	return string(b)
}

// addXY files a change under staged and/or unstaged from its XY status pair.
func (s *Snapshot) addXY(xy, path string) {
	if len(xy) != 2 {
		return
	}
	if xy[0] != '.' {
		s.Staged = append(s.Staged, FileChange{Path: path, Status: xy[0]})
	}
	if xy[1] != '.' {
		s.Unstaged = append(s.Unstaged, FileChange{Path: path, Status: xy[1]})
	}
}

// parseNumstat parses `git diff --numstat -z`. Ordinary records are
// "<added>\t<deleted>\t<path>\0". Renames have an empty path in that first
// record followed by "<old>\0<new>\0"; the new path is used. NUL framing is
// unambiguous for filenames containing tabs, newlines, quotes, or the literal
// text " => ". Binary files show "-" and count as 0.
func parseNumstat(out string) map[string][2]int {
	m := make(map[string][2]int)
	records := strings.Split(out, "\x00")
	for i := 0; i < len(records)-1; i++ {
		f := strings.SplitN(records[i], "\t", 3)
		if len(f) != 3 {
			continue
		}
		add, _ := strconv.Atoi(f[0])
		del, _ := strconv.Atoi(f[1])
		path := f[2]
		if path == "" {
			if i+2 >= len(records) {
				continue
			}
			i += 2 // skip old path and land on the new path
			path = records[i]
		}
		m[path] = [2]int{add, del}
	}
	return m
}

func applyCounts(files []FileChange, counts map[string][2]int) {
	for i := range files {
		if c, ok := counts[files[i].Path]; ok {
			files[i].Added, files[i].Deleted = c[0], c[1]
		}
	}
}
