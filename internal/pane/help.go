package pane

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
	"github.com/sarcasticbird/wrap/internal/config"
)

var headingStyle = lipgloss.NewStyle().Bold(true)

// FocusLabel renders tmux's Meta-key syntax using the Option symbol already
// used by wrap's UI. Other tmux key syntax remains unchanged.
func FocusLabel(key string) string {
	if strings.HasPrefix(key, "M-") {
		return "⌥" + strings.TrimPrefix(key, "M-")
	}
	return key
}

// Heading labels one of wrap's list panes with its effective focus binding.
func Heading(name, key string, width int) string {
	return headingStyle.Render(clip(name+" ("+FocusLabel(key)+")", width))
}

// ActionFooter is the compact always-visible workspace action reference.
func ActionFooter(width int) string {
	return DimStyle.Render(clip("h help · q detach · Q shutdown", width))
}

// HelpFooter names every key that closes the pane-local Help view.
func HelpFooter(width int) string {
	return DimStyle.Render(clip("h / esc / q close", width))
}

// HelpBody renders the shared shortcut reference for both list panes.
func HelpBody(keys config.Keys, width, height int) string {
	if height == 0 {
		return ""
	}
	keys = keys.WithDefaults()
	lines := []string{
		"Help",
		fmt.Sprintf(
			"focus  %s terminal · %s Git · %s Terminals",
			FocusLabel(keys.FocusTerminal),
			FocusLabel(keys.FocusTree),
			FocusLabel(keys.FocusTerms),
		),
		"move   ↑/↓ or j/k · ←/→ collapse/expand · ↵ open",
		"Git    ↵ select/open · ←/→ files · x kill",
		"terms  ↵ show · ←/→ details · n new/bind · r rename · x kill",
		"wrap   q detach · Q shutdown",
	}
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = clip(lines[i], width)
		if i == 0 {
			lines[i] = headingStyle.Render(lines[i])
		} else {
			lines[i] = DimStyle.Render(lines[i])
		}
	}
	return strings.Join(lines, "\n")
}

func clip(s string, width int) string {
	if width <= 0 {
		return s
	}
	return runewidth.Truncate(s, width, "")
}
