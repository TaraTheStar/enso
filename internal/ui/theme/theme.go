// SPDX-License-Identifier: AGPL-3.0-or-later

// Package theme holds the framework-agnostic colour palette. It
// produces RGB Colors and a name→Color map; the bubble backend
// translates these into lipgloss.Color via Color.Hex(). User overrides
// come from `$XDG_CONFIG_HOME/enso/theme.toml`.
package theme

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"

	"github.com/TaraTheStar/enso/internal/paths"
)

// Color is an RGB triple. Backends render via Hex().
type Color struct {
	R, G, B int32
}

// Hex returns "#rrggbb".
func (c Color) Hex() string {
	return fmt.Sprintf("#%02x%02x%02x", c.R&0xff, c.G&0xff, c.B&0xff)
}

// Palette is a name → Color map. Names are lowercase ASCII; both the
// bundled palette and user overrides normalise to lowercase before
// insertion.
type Palette map[string]Color

// Default returns enso's bundled muted-pastel palette.
//
// Design constraints: pastels at ~25–40% saturation, two distinct
// purples (mauve = pinker, lavender = bluer/hero), warnings stay on
// dust (never confused with types), errors are pastel-rose. User themes
// ($XDG_CONFIG_HOME/enso/theme.toml) still win: this is just the baseline.
//
// The palette includes the standard names (yellow/teal/red/gray/green)
// alongside the accents (mauve/lavender/comment/dust/sage) so generic
// colour references work uniformly.
func Default() Palette {
	return Palette{
		// Two-purple split: distinct hues so user vs. assistant pills
		// read as different voices without collapsing into "more purple".
		"mauve":    {0xc0, 0x90, 0xdc}, // user pill (pinker purple)
		"lavender": {0xa0, 0x98, 0xf4}, // assistant pill (bluer / hero)
		"comment":  {0x72, 0x68, 0xa0}, // reasoning bar (recedes)
		"dust":     {0xbc, 0xc0, 0x8c}, // warnings only (never types)
		"sage":     {0x9c, 0xc4, 0xa4}, // success / additions

		// Standard names retuned to palette.
		"yellow": {0xbc, 0xc0, 0x8c}, // = dust
		"teal":   {0x6c, 0xa4, 0xc8}, // tools, info chips, types
		"red":    {0xe8, 0xbf, 0xd2}, // active errors (bright_red)
		"gray":   {0xb8, 0xba, 0xba}, // neutral subtext
		"green":  {0x9c, 0xc4, 0xa4}, // = sage
	}
}

// LoadFromFile reads a TOML file with a `[colors]` table and returns
// the overrides as a Palette. Missing file returns an empty palette
// and a nil error — "no theme — use defaults" is the right behaviour
// for first-run and for users who never touched theme config.
//
// Format:
//
//	[colors]
//	yellow = "#ffd866"
//	teal   = "#78dce8"
//	gray   = "#727072"
//	red    = "#ff6188"
//
// Bad hex values surface as errors so the user notices typos.
func LoadFromFile(path string) (Palette, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Palette{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var t struct {
		Colors map[string]string `toml:"colors"`
	}
	if err := toml.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make(Palette, len(t.Colors))
	for name, hex := range t.Colors {
		c, err := parseHex(hex)
		if err != nil {
			return nil, fmt.Errorf("color %q: %w", name, err)
		}
		out[strings.ToLower(name)] = c
	}
	return out, nil
}

// DefaultPath returns $XDG_CONFIG_HOME/enso/theme.toml.
func DefaultPath() (string, error) {
	dir, err := paths.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "theme.toml"), nil
}

// parseHex converts "#rrggbb" or "rrggbb" into a Color. Anything else
// is rejected.
func parseHex(s string) (Color, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	if len(s) != 6 {
		return Color{}, fmt.Errorf("expected #rrggbb (6 hex digits), got %q", s)
	}
	rgb, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		return Color{}, fmt.Errorf("parse hex: %w", err)
	}
	return Color{
		R: int32(rgb>>16) & 0xff,
		G: int32(rgb>>8) & 0xff,
		B: int32(rgb) & 0xff,
	}, nil
}
