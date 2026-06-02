// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/TaraTheStar/enso/internal/slash"
)

// slashPaletteData backs the `/`-trigger command palette overlay. It's
// the discoverability surface for slash commands (U2): typing `/` on an
// empty input line opens a filtered, navigable list of every registered
// command with its one-line description, instead of forcing the user to
// remember names or scroll the flat `/help` wall.
//
// Same alt-screen modal pattern as the @ file picker (picker.go): the
// struct is held by the model and reused across opens; only filter/sel
// are per-open state. The registry is the single source of truth so the
// palette automatically reflects built-ins, user/project commands, and
// loaded skills.
type slashPaletteData struct {
	reg *slash.Registry

	// Per-open state.
	filter string // text typed after the implied leading '/'
	sel    int
}

// paletteEntry is one row in the palette: the command name (no leading
// slash) and its description.
type paletteEntry struct {
	name string
	desc string
}

// reset clears per-open state. Called each time the palette opens.
func (p *slashPaletteData) reset() {
	p.filter = ""
	p.sel = 0
}

// matches returns the filtered command list for the current filter.
// Registry.List() is already name-sorted; we partition into prefix
// matches (shown first, the common case — you type the start of a name)
// then substring matches, preserving alpha order within each group. An
// empty filter lists everything.
func (p *slashPaletteData) matches() []paletteEntry {
	f := strings.ToLower(strings.TrimSpace(p.filter))
	var prefix, sub []paletteEntry
	for _, c := range p.reg.List() {
		name := c.Name()
		ln := strings.ToLower(name)
		e := paletteEntry{name: name, desc: c.Description()}
		switch {
		case f == "" || strings.HasPrefix(ln, f):
			prefix = append(prefix, e)
		case strings.Contains(ln, f):
			sub = append(sub, e)
		}
	}
	return append(prefix, sub...)
}

// renderSlashPalette produces the alt-screen view for the command
// palette. Mirrors renderPicker's layout (incl. height-bounded
// scrolling) so the two overlays feel identical; width is ignored and
// the terminal handles wrapping.
func renderSlashPalette(p *slashPaletteData, width, height int) string {
	_ = width

	title := lipgloss.NewStyle().
		Foreground(paletteHex("lavender")).
		Bold(true).
		Render("/ run a command")

	filterLabel := statusStyle.Render("/")
	cursor := lipgloss.NewStyle().Reverse(true).Render(" ")
	filter := filterLabel + p.filter + cursor

	matches := p.matches()
	var listLines []string
	if len(matches) == 0 {
		listLines = []string{statusStyle.Render("(no matches)")}
	} else {
		// Pad names to a common width so descriptions align in a column.
		nameW := 0
		for _, e := range matches {
			if n := len(e.name); n > nameW {
				nameW = n
			}
		}
		for i, e := range matches {
			name := fmt.Sprintf("/%-*s", nameW, e.name)
			desc := lipgloss.NewStyle().Foreground(paletteHex("comment")).Render("  " + e.desc)
			line := name + desc
			if i == p.sel {
				line = lipgloss.NewStyle().
					Foreground(paletteHex("mauve")).
					Bold(true).
					Render("› "+name) + desc
			} else {
				line = "  " + line
			}
			listLines = append(listLines, line)
		}
	}

	listLines, above, below := windowList(listLines, p.sel, height-overlayChrome)

	footer := lipgloss.NewStyle().
		Foreground(paletteHex("comment")).
		Faint(true).
		Render(fmt.Sprintf("Enter pick · ↑/↓ move · Esc cancel    %d command%s", len(matches), plural(len(matches))) + scrollSuffix(above, below))

	return lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		filter,
		"",
		strings.Join(listLines, "\n"),
		"",
		footer,
	)
}
