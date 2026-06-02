// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import "fmt"

// Fixed (non-list) row counts for the scrolling overlays, so the list
// budget is height minus this chrome. The picker and slash palette share
// a layout — title, blank, filter, blank, <list>, blank, footer — while
// the sessions overlay has no filter line. A 1-row safety margin keeps
// the alt-screen view from exactly filling the terminal (which can nudge
// it to scroll).
const (
	overlayChrome         = 7 // title+blank+filter+blank+blank+footer + 1 margin
	sessionsOverlayChrome = 5 // title+blank+blank+footer + 1 margin
)

// windowList returns the slice of `lines` that fits in a scrolling
// viewport of at most maxRows rows while keeping index `sel` on-screen,
// plus the counts hidden above and below the window. When everything
// fits (or maxRows <= 0, e.g. the terminal height isn't known yet — the
// case in unit tests that construct a bare model) it returns the full
// list with above/below == 0, so callers degrade to the old
// render-everything behaviour rather than clipping to nothing.
//
// The selection is kept roughly centred: the window starts maxRows/2
// rows above sel, clamped to the list bounds. This means paging up/down
// past the edge of the viewport scrolls the list instead of letting the
// cursor run off-screen (the bug this fixes — overflow on short
// terminals).
func windowList(lines []string, sel, maxRows int) (visible []string, above, below int) {
	n := len(lines)
	if maxRows <= 0 || n <= maxRows {
		return lines, 0, 0
	}
	if sel < 0 {
		sel = 0
	}
	if sel >= n {
		sel = n - 1
	}
	start := sel - maxRows/2
	if start < 0 {
		start = 0
	}
	if start+maxRows > n {
		start = n - maxRows
	}
	end := start + maxRows
	return lines[start:end], start, n - end
}

// scrollSuffix renders a compact "  ↑N ↓M" tail describing how many rows
// are hidden above/below the current window, for appending to an
// overlay's footer so the user knows the list scrolls. Empty when
// nothing is hidden.
func scrollSuffix(above, below int) string {
	switch {
	case above > 0 && below > 0:
		return fmt.Sprintf("    ↑%d ↓%d more", above, below)
	case above > 0:
		return fmt.Sprintf("    ↑%d more", above)
	case below > 0:
		return fmt.Sprintf("    ↓%d more", below)
	default:
		return ""
	}
}
