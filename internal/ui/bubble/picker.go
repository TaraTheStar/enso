// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/TaraTheStar/enso/internal/picker"
)

// pickerData is the @-trigger file picker overlay's state. Fields
// mutate as the user types into the filter and navigates the list.
// pickerData is held by the model, not constructed per-open, so the
// underlying file list is walked once and reused across opens.
type pickerData struct {
	cwd            string
	extraDirs      []string
	ignorePatterns []string

	files  []string // full file list, walked at first open
	walked bool     // lazy: don't walk on startup, only when first @ fires

	// Per-open state.
	filter string
	sel    int
}

// pickerOpen reports whether the file picker overlay is currently
// active. The model's own bool tracks this; pickerData itself is
// populated even when closed.
func (p *pickerData) reset() {
	p.filter = ""
	p.sel = 0
}

// ensureWalked populates p.files lazily on first open. Walking the tree
// is fast for small repos but O(n) — deferring until @ fires keeps
// startup zero-cost.
func (p *pickerData) ensureWalked() {
	if p.walked {
		return
	}
	files, err := picker.WalkAll(p.cwd, p.extraDirs, p.ignorePatterns)
	if err != nil {
		// Walking failures (permission denied on some subdirs) are
		// non-fatal; show whatever WalkAll managed to enumerate.
		_ = err
	}
	p.files = files
	p.walked = true
}

// matches returns the filtered, ranked file list for the current
// filter text. Cap at a sensible page size — beyond ~20 entries the
// list is more confusing than helpful in a small overlay.
func (p *pickerData) matches() []string {
	const limit = 50
	return picker.Rank(p.files, p.filter, limit)
}

// renderPicker produces the alt-screen view for the @ file picker.
// width/height come from tea.WindowSizeMsg; phase-4 ignores them and
// lets the terminal handle wrapping at narrow widths.
func renderPicker(p *pickerData, width, height int) string {
	_ = width
	_ = height

	title := lipgloss.NewStyle().
		Foreground(paletteHex("lavender")).
		Bold(true).
		Render("@ pick a file")

	filterLabel := statusStyle.Render("filter: ")
	cursor := lipgloss.NewStyle().Reverse(true).Render(" ")
	filter := filterLabel + p.filter + cursor

	matches := p.matches()
	var listLines []string
	if len(matches) == 0 {
		listLines = []string{statusStyle.Render("(no matches)")}
	} else {
		for i, f := range matches {
			line := f
			if i == p.sel {
				line = lipgloss.NewStyle().
					Foreground(paletteHex("mauve")).
					Bold(true).
					Render("› ") + line
			} else {
				line = "  " + line
			}
			listLines = append(listLines, line)
		}
	}

	footer := lipgloss.NewStyle().
		Foreground(paletteHex("comment")).
		Faint(true).
		Render(fmt.Sprintf("Enter pick · ↑/↓ move · Esc cancel    %d match%s", len(matches), plural(len(matches))))

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
