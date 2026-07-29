// Package config loads and validates ~/.config/wrap/wrap.toml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/BurntSushi/toml"
)

const HomeSession = "wrap-home"

type Config struct {
	Defaults  Defaults `toml:"defaults"`
	Keys      Keys     `toml:"keys"`
	WalkDepth int      `toml:"walk_depth"`
	// TreeSide is which column the tree/terms pair renders in: "left"
	// (default) or "right" (mirrored — terminal on the left instead).
	TreeSide string `toml:"tree_side"`
	// TreeWidth is the tree/terms column's width as a percent of the
	// window (default 25, clamped to [10, 60]).
	TreeWidth int `toml:"tree_width"`
}

// Defaults mirrors every top-level knob: users keep putting knobs inside
// [defaults] (a natural TOML reading), and BurntSushi drops unknown table
// keys silently — so each knob is accepted in both positions, with the
// top level winning when both are set. See finalize.
type Defaults struct {
	Cmd       string `toml:"cmd"`
	WalkDepth int    `toml:"walk_depth"`
	TreeSide  string `toml:"tree_side"`
	TreeWidth int    `toml:"tree_width"`
}

// Keys are the UI-server pane-focus bindings (tmux key syntax).
type Keys struct {
	FocusTree     string `toml:"focus_tree"`
	FocusTerminal string `toml:"focus_terminal"`
	FocusTerms    string `toml:"focus_terms"`
}

func (k Keys) WithDefaults() Keys {
	if k.FocusTree == "" {
		k.FocusTree = "M-1"
	}
	if k.FocusTerminal == "" {
		k.FocusTerminal = "M-2"
	}
	if k.FocusTerms == "" {
		k.FocusTerms = "M-3"
	}
	return k
}

// DefaultPath returns $XDG_CONFIG_HOME/wrap/wrap.toml, falling back to
// ~/.config/wrap/wrap.toml.
func DefaultPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); dir != "" {
		return filepath.Join(dir, "wrap", "wrap.toml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "wrap", "wrap.toml"), nil
}

// Load parses the config. The second return lists warnings for keys the
// parser did not recognize — misplaced or misspelled knobs previously
// no-opped silently, which cost real debugging time three separate times.
func Load(path string) (*Config, []string, error) {
	var c Config
	md, err := toml.DecodeFile(path, &c)
	if err != nil {
		return nil, nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var warns []string
	for _, k := range md.Undecoded() {
		warns = append(warns, fmt.Sprintf("config: unknown key %q ignored", k.String()))
	}
	if err := c.finalize(
		md.IsDefined("walk_depth"),
		md.IsDefined("tree_side"),
		md.IsDefined("tree_width"),
		md.IsDefined("defaults", "tree_width"),
	); err != nil {
		return nil, warns, err
	}
	return &c, warns, nil
}

// finalize clamps WalkDepth to [1, 5], defaults/clamps TreeWidth, and
// normalizes/validates TreeSide. TreeWidth is clamped silently (house
// precedent: WalkDepth clamps rather than errors); TreeSide is validated
// instead — an unrecognized value is far more likely a typo the user
// wants surfaced than a value worth silently coercing.
func (c *Config) finalize(walkDepthDefined, treeSideDefined, treeWidthDefined, defaultsTreeWidthDefined bool) error {
	if !walkDepthDefined && c.Defaults.WalkDepth != 0 {
		c.WalkDepth = c.Defaults.WalkDepth
	}
	if !treeSideDefined && c.Defaults.TreeSide != "" {
		c.TreeSide = c.Defaults.TreeSide
	}
	if !treeWidthDefined && defaultsTreeWidthDefined {
		c.TreeWidth = c.Defaults.TreeWidth
	}
	if c.WalkDepth < 1 {
		c.WalkDepth = 1
	}
	if c.WalkDepth > 5 {
		c.WalkDepth = 5
	}

	if !treeWidthDefined && !defaultsTreeWidthDefined && c.TreeWidth == 0 {
		c.TreeWidth = 25
	}
	if c.TreeWidth < 10 {
		c.TreeWidth = 10
	}
	if c.TreeWidth > 60 {
		c.TreeWidth = 60
	}

	switch c.TreeSide {
	case "":
		c.TreeSide = "left"
	case "left", "right":
	default:
		return fmt.Errorf("tree_side: invalid value %q (must be \"left\" or \"right\")", c.TreeSide)
	}
	return nil
}

// tmux session names cannot contain "." or ":"; spaces make targeting
// awkward. All three become "_".
var nameSanitizer = strings.NewReplacer(".", "_", ":", "_", " ", "_")

// SanitizeName makes s safe to use as a tmux session name (or as one
// slash-separated component of one). It is the single definition of that
// rule: workspace names and session components must agree on it, or
// SessionOwnedBy stops matching sessions the tree creates and workspace
// scoping breaks silently. It is idempotent.
func SanitizeName(s string) string {
	return nameSanitizer.Replace(s)
}

func ValidateWorkspaceName(name string) error {
	switch {
	case name == "":
		return errors.New("workspace name is empty")
	case name == "." || name == "..":
		return fmt.Errorf("workspace name %q is not allowed", name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("workspace name %q contains a path separator", name)
	case strings.Contains(name, "$"):
		return fmt.Errorf("workspace name %q contains '$', which tmux 3.2 through 3.4 cannot preserve", name)
	case !utf8.ValidString(name):
		return fmt.Errorf("workspace name %q is not valid UTF-8", name)
	case strings.IndexFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }) >= 0:
		return fmt.Errorf("workspace name %q contains a non-printable character", name)
	case name == HomeSession:
		return fmt.Errorf("workspace name %q is reserved; rename the folder or open its parent", name)
	case strings.Contains(name, termInfix) || strings.HasSuffix(name, termStem) || strings.HasSuffix(name, diffSuffix):
		return fmt.Errorf("workspace name %q overlaps a reserved session namespace; rename the folder or open its parent", name)
	default:
		return nil
	}
}

// SessionName builds the session name for an entry within a workspace.
// Entries use a reversible byte encoding instead of SanitizeName: replacing
// punctuation with "_" made distinct repository names share one tmux session.
func SessionName(ws, entry string) string {
	return SanitizeName(ws) + "/" + encodeSessionComponent(entry)
}

// LegacySessionName returns the entry-session format used before reversible
// encoding was introduced. It exists only for startup migration; new sessions
// must always use SessionName.
func LegacySessionName(ws, entry string) string {
	return SanitizeName(ws) + "/" + SanitizeName(entry)
}

func encodeSessionComponent(s string) string {
	const hex = "0123456789abcdef"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c >= '0' && c <= '9' || c == '_' || c == '-' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('~')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}

// The delimiters that separate a workspace name from what kind of session
// it owns. U+00B7 keeps them visually distinct from the "/" used for named
// entries, and cannot appear in a sanitized name. Every producer and the
// SessionOwnedBy parser below build on these — they must agree, or a
// session wrap created stops being recognized as its own.
const (
	termStem   = "·term"
	termInfix  = termStem + "·"
	diffSuffix = "·diff"
)

// TermPrefix is the prefix shared by workspace ws's free terminals; its
// scratch terminals are TermPrefix(ws) + a number or a slugified label.
func TermPrefix(ws string) string { return ws + termInfix }

// DiffSession is the name of workspace ws's one-shot diff pager session.
func DiffSession(ws string) string { return ws + diffSuffix }

// IsTermSession reports whether name is one of workspace ws's free
// terminals — the ones that may be renamed.
func IsTermSession(ws, name string) bool {
	return strings.HasPrefix(name, TermPrefix(ws))
}

// SessionOwnedBy reports whether session name belongs to workspace ws: the
// root session itself, one of its named entries (ws/entry), one of its
// free terminals (ws·term·N), or its one-shot diff pager (ws·diff). Other
// workspaces (including wrap-home) are excluded — a workspace name sharing
// a prefix without the delimiter (e.g. "wsextra" against "ws") is not a
// match.
func SessionOwnedBy(ws, name string) bool {
	return name == ws || name == DiffSession(ws) ||
		strings.HasPrefix(name, ws+"/") || IsTermSession(ws, name)
}
