// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gdamore/tcell/v2"
	"github.com/pelletier/go-toml/v2"
	"github.com/rivo/tview"
)

// ApplyDefaultPalette retunes tcell's named-color table to enso's
// bundled muted-pastel palette and registers the accent names the chat
// renderer uses for role pills (`mauve`, `lavender`, `comment`, `dust`,
// `sage`). Idempotent and safe to call once at startup before any tview
// widget is created.
//
// Design constraints: pastels at ~25–40% saturation, two distinct
// purples (mauve = pinker, lavender = bluer/hero), warnings stay on
// dust (never confused with types), errors are pastel-rose. User themes
// (~/.enso/theme.toml) still win: this is just the baseline.
func ApplyDefaultPalette() {
	overrides := map[string]tcell.Color{
		// Two-purple split: distinct hues so user vs. assistant pills
		// read as different voices without collapsing into "more purple".
		"mauve":    tcell.NewRGBColor(0xc0, 0x90, 0xdc), // user pill (pinker purple)
		"lavender": tcell.NewRGBColor(0xa0, 0x98, 0xf4), // assistant pill (bluer / hero)
		"comment":  tcell.NewRGBColor(0x72, 0x68, 0xa0), // reasoning bar (recedes)
		"dust":     tcell.NewRGBColor(0xbc, 0xc0, 0x8c), // warnings only (never types)
		"sage":     tcell.NewRGBColor(0x9c, 0xc4, 0xa4), // success / additions

		// Standard tcell names retuned to match the palette so existing
		// [yellow]/[teal]/[gray]/[red]/[green] tags pick up the new look
		// without touching every callsite.
		"yellow": tcell.NewRGBColor(0xbc, 0xc0, 0x8c), // = dust
		"teal":   tcell.NewRGBColor(0x6c, 0xa4, 0xc8), // tools, info chips, types
		"red":    tcell.NewRGBColor(0xe8, 0xbf, 0xd2), // active errors (bright_red)
		"gray":   tcell.NewRGBColor(0xb8, 0xba, 0xba), // neutral subtext
		"green":  tcell.NewRGBColor(0x9c, 0xc4, 0xa4), // = sage
	}
	for name, c := range overrides {
		tcell.ColorNames[name] = c
	}

	// Pin tview's global Styles so any widget that *doesn't* explicitly
	// set a color picks up palette-coherent defaults instead of tview's
	// hardcoded green/yellow/blue. Most call sites already override the
	// fields they actually use; this is defense-in-depth for code paths
	// we don't currently exercise (Form labels, InputField autocomplete,
	// generic borders/titles, …).
	//
	// PrimitiveBackgroundColor = ColorDefault is the load-bearing one:
	// every widget paints its background with this color, so leaving the
	// default (ColorBlack) means every pane shows a solid black rectangle
	// regardless of the user's terminal background. ColorDefault lets the
	// terminal's bg show through.
	tview.Styles.PrimitiveBackgroundColor = tcell.ColorDefault
	tview.Styles.ContrastBackgroundColor = tcell.ColorDefault
	tview.Styles.MoreContrastBackgroundColor = tcell.GetColor("comment")
	tview.Styles.BorderColor = tcell.GetColor("comment")
	tview.Styles.TitleColor = tcell.GetColor("lavender")
	tview.Styles.GraphicsColor = tcell.GetColor("comment")
	tview.Styles.PrimaryTextColor = tcell.GetColor("white")
	tview.Styles.SecondaryTextColor = tcell.GetColor("comment")
	tview.Styles.TertiaryTextColor = tcell.GetColor("comment")
	tview.Styles.InverseTextColor = tcell.GetColor("black")
	tview.Styles.ContrastSecondaryTextColor = tcell.GetColor("comment")
}

// LoadTheme reads a TOML file with a `[colors]` table and overrides the
// matching entries in tcell.ColorNames. Each value is `#rrggbb`. Both
// `[name]`-style tview tags and direct `tcell.GetColor("name")` lookups
// pick up the override automatically. Names not listed retain their tcell
// defaults.
//
// Format:
//
//	[colors]
//	yellow = "#ffd866"
//	teal   = "#78dce8"
//	gray   = "#727072"
//	red    = "#ff6188"
//
// Missing file is treated as "no theme — use defaults" and returns nil.
// Bad hex values surface as errors so the user notices typos.
func LoadTheme(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read %s: %w", path, err)
	}
	var t struct {
		Colors map[string]string `toml:"colors"`
	}
	if err := toml.Unmarshal(data, &t); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	for name, hex := range t.Colors {
		c, err := parseHexColor(hex)
		if err != nil {
			return fmt.Errorf("color %q: %w", name, err)
		}
		tcell.ColorNames[strings.ToLower(name)] = c
	}
	return nil
}

// DefaultThemePath returns ~/.enso/theme.toml.
func DefaultThemePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".enso", "theme.toml"), nil
}

// parseHexColor converts "#rrggbb" or "rrggbb" into a tcell.Color via
// NewRGBColor. Anything else is rejected.
func parseHexColor(s string) (tcell.Color, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return 0, fmt.Errorf("expected #rrggbb (6 hex digits), got %q", s)
	}
	rgb, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("parse hex: %w", err)
	}
	return tcell.NewRGBColor(
		int32(rgb>>16)&0xff,
		int32(rgb>>8)&0xff,
		int32(rgb)&0xff,
	), nil
}
