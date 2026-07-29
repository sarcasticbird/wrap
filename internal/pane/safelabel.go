package pane

import "strconv"

// SafeLabel renders an untrusted name — a filename, directory, or branch
// discovered from a repository — safe to write to the terminal. A hostile
// repo can name a file with raw escape bytes (git C-quotes them, and wrap's
// unquoter faithfully rebuilds them), which would otherwise reach the outer
// terminal verbatim; with set-clipboard on, an embedded OSC 52 would hijack
// the system clipboard on a single keystroke. strconv.QuoteToGraphic keeps
// printable Unicode intact and escapes every control byte to its \x.. form;
// the surrounding quotes it adds are stripped so the label reads naturally.
func SafeLabel(s string) string {
	quoted := strconv.QuoteToGraphic(s)
	return quoted[1 : len(quoted)-1]
}
