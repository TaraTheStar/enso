// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/TaraTheStar/enso/internal/slash"
)

// fakeCmd is a minimal slash.Command for palette tests.
type fakeCmd struct {
	name string
	desc string
}

func (c fakeCmd) Name() string                      { return c.name }
func (c fakeCmd) Description() string               { return c.desc }
func (c fakeCmd) Run(context.Context, string) error { return nil }

func paletteReg() *slash.Registry {
	reg := slash.NewRegistry()
	reg.Register(fakeCmd{"help", "list available slash commands"})
	reg.Register(fakeCmd{"model", "switch the active model"})
	reg.Register(fakeCmd{"compact", "compact the conversation"})
	return reg
}

// TestSlashPalette_Matches covers the filter ranking: empty filter lists
// everything (name-sorted), a prefix narrows to prefix hits first, and a
// substring that isn't a prefix still matches.
func TestSlashPalette_Matches(t *testing.T) {
	p := &slashPaletteData{reg: paletteReg()}

	if got := len(p.matches()); got != 3 {
		t.Fatalf("empty filter should list all 3 commands; got %d", got)
	}

	p.filter = "mo"
	m := p.matches()
	if len(m) != 1 || m[0].name != "model" {
		t.Fatalf("prefix 'mo' should match only model; got %+v", m)
	}

	// "pac" is a substring of "compact" but not a prefix — still a match.
	p.filter = "pac"
	m = p.matches()
	if len(m) != 1 || m[0].name != "compact" {
		t.Fatalf("substring 'pac' should match compact; got %+v", m)
	}

	p.filter = "zzz"
	if got := len(p.matches()); got != 0 {
		t.Fatalf("no-match filter should be empty; got %d", got)
	}
}

// TestSlashPalette_TriggerAndAccept is the U2 round-trip: typing `/` on
// an empty line opens the palette (without inserting the `/`), filtering
// + Enter inserts `/<name> ` into the input so the user can add args.
func TestSlashPalette_TriggerAndAccept(t *testing.T) {
	m := &model{palette: &slashPaletteData{reg: paletteReg()}}

	// `/` on an empty line opens the palette and is not inserted.
	m.handleKey(tea.KeyPressMsg{Code: '/', Text: "/"})
	if !m.paletteOpen {
		t.Fatal("typing / on an empty line should open the palette")
	}
	if m.input.buf != "" {
		t.Fatalf("the trigger / should not be inserted; buf=%q", m.input.buf)
	}

	// Filter to "model" and accept.
	for _, r := range "mod" {
		m.handleKey(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.paletteOpen {
		t.Fatal("Enter should close the palette")
	}
	if m.input.buf != "/model " {
		t.Fatalf("accept should insert the command; buf=%q", m.input.buf)
	}
}

// TestSlashPalette_NotTriggeredMidLine: `/` is only a trigger on an empty
// line. Inside an existing message (e.g. a path) it inserts literally.
func TestSlashPalette_NotTriggeredMidLine(t *testing.T) {
	m := &model{palette: &slashPaletteData{reg: paletteReg()}}
	m.input.buf = "see src"
	m.input.cursor = len(m.input.buf)

	m.handleKey(tea.KeyPressMsg{Code: '/', Text: "/"})
	if m.paletteOpen {
		t.Fatal("/ mid-line should not open the palette")
	}
	if m.input.buf != "see src/" {
		t.Fatalf("/ mid-line should insert literally; buf=%q", m.input.buf)
	}
}

// TestSlashPalette_EscAndBackspaceClose: Esc cancels; backspace past the
// empty filter removes the implied `/` and dismisses the palette.
func TestSlashPalette_EscAndBackspaceClose(t *testing.T) {
	// Esc.
	m := &model{palette: &slashPaletteData{reg: paletteReg()}, paletteOpen: true}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.paletteOpen {
		t.Fatal("Esc should close the palette")
	}
	if m.input.buf != "" {
		t.Fatalf("Esc should leave the buffer empty; buf=%q", m.input.buf)
	}

	// Backspace on an empty filter.
	m = &model{palette: &slashPaletteData{reg: paletteReg()}, paletteOpen: true}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyBackspace})
	if m.paletteOpen {
		t.Fatal("backspace past empty filter should close the palette")
	}
}

// TestSlashPalette_View renders the overlay while open and lists commands
// with their descriptions.
func TestSlashPalette_View(t *testing.T) {
	m := &model{palette: &slashPaletteData{reg: paletteReg()}, paletteOpen: true}
	got := viewText(m)
	if !strings.Contains(got, "run a command") {
		t.Fatalf("palette view should show its title; got:\n%s", got)
	}
	if !strings.Contains(got, "/model") || !strings.Contains(got, "switch the active model") {
		t.Fatalf("palette view should list commands with descriptions; got:\n%s", got)
	}
}
