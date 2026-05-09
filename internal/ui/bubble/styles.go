// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"image/color"
	"log/slog"

	"charm.land/lipgloss/v2"

	"github.com/TaraTheStar/enso/internal/ui/theme"
)

// styles are the Lipgloss styles the model and run.go use to render
// the live region and the scrollback-bound message lines. They're
// resolved from the shared theme palette (internal/ui/theme) so user
// overrides in ~/.enso/theme.toml apply to bubble exactly as they do
// to tview.
//
// Initialised at run.go startup via newStyles. Tests use defaultStyles.
var (
	streamStyle    lipgloss.Style
	statusStyle    lipgloss.Style
	userStyle      lipgloss.Style
	asstStyle      lipgloss.Style
	promptStyle    lipgloss.Style
	errorStyle     lipgloss.Style
	noticeStyle    lipgloss.Style
	spinHeadStyle  lipgloss.Style
	spinTrailStyle lipgloss.Style
	spinDimStyle   lipgloss.Style
	diffAddStyle   lipgloss.Style
	diffDelStyle   lipgloss.Style
	diffHunkStyle  lipgloss.Style
	diffFileStyle  lipgloss.Style
	codeBarStyle   lipgloss.Style

	// currentPalette holds the most recently applied palette so the
	// glamour markdown renderer can build its theme from the same
	// source as every other style. Updated by applyStyles.
	currentPalette theme.Palette
)

func init() {
	// Default-palette styles so package init produces working values
	// (used by model_test.go and any other early-render path). run.go
	// re-applies after loading the user's theme.toml.
	applyStyles(theme.Default())
}

// loadAndApplyTheme reads ~/.enso/theme.toml (or the default path) and
// re-applies the bubble package's lipgloss styles with the merged
// palette. Errors loading the theme are logged but non-fatal — a typo
// in theme.toml shouldn't block the TUI.
func loadAndApplyTheme() {
	pal := theme.Default()
	if path, err := theme.DefaultPath(); err == nil {
		if overrides, err := theme.LoadFromFile(path); err != nil {
			slog.Warn("theme load", "path", path, "err", err)
		} else {
			for name, c := range overrides {
				pal[name] = c
			}
		}
	}
	applyStyles(pal)
}

func applyStyles(pal theme.Palette) {
	currentPalette = pal
	invalidateMarkdownRenderers()
	hex := func(name string) color.Color {
		if c, ok := pal[name]; ok {
			return lipgloss.Color(c.Hex())
		}
		return lipgloss.Color("")
	}
	// Pure body text: no colour override — use the terminal's
	// foreground so it stays neutral.
	streamStyle = lipgloss.NewStyle()
	// Subtitles, idle status, scrollback annotations recede.
	statusStyle = lipgloss.NewStyle().Foreground(hex("comment")).Faint(true)
	// Role prefixes: mauve for user (pinker purple), lavender for
	// assistant (bluer / hero) — matches the tview pill colours so
	// users moving between backends see the same identity.
	userStyle = lipgloss.NewStyle().Foreground(hex("mauve")).Bold(true)
	asstStyle = lipgloss.NewStyle().Foreground(hex("lavender")).Bold(true)
	promptStyle = lipgloss.NewStyle().Foreground(hex("comment"))
	errorStyle = lipgloss.NewStyle().Foreground(hex("red"))
	// Notices use dust/yellow — warnings only, never types.
	noticeStyle = lipgloss.NewStyle().Foreground(hex("dust"))
	// Comet-trail spinner cells. Bright head and trail share the
	// assistant accent (lavender) so the spinner reads as part of the
	// model's voice; the trail cell is faint so the head visually
	// leads. The dim background cells use the same comment colour as
	// the rest of the recede status line so the band doesn't feel like
	// a foreign element.
	spinHeadStyle = lipgloss.NewStyle().Foreground(hex("lavender")).Bold(true)
	spinTrailStyle = lipgloss.NewStyle().Foreground(hex("lavender")).Faint(true)
	spinDimStyle = lipgloss.NewStyle().Foreground(hex("comment")).Faint(true)
	// Diff coloring on tool output: sage for additions / red for
	// deletions matches enso's existing semantic palette (sage =
	// success, red = error). Hunk and file headers use teal (info)
	// and a bold neutral so they read as structure, not content.
	diffAddStyle = lipgloss.NewStyle().Foreground(hex("sage"))
	diffDelStyle = lipgloss.NewStyle().Foreground(hex("red"))
	diffHunkStyle = lipgloss.NewStyle().Foreground(hex("teal"))
	diffFileStyle = lipgloss.NewStyle().Foreground(hex("gray")).Bold(true)
	// Code-block left bar in assistant text: a light vertical in gray
	// that recedes against prose but cleanly delimits code from words.
	// Uses a different glyph from the reasoning bar (▎) so the two
	// kinds of "structured assistant content" remain visually distinct.
	codeBarStyle = lipgloss.NewStyle().Foreground(hex("gray"))
}
