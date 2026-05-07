// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"strings"

	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/session"
)

// splashLogo is the ensō (円相) brushstroke rendered as a small unicode
// glyph + tagline. Kept short so it doesn't dominate a fresh chat view.
const splashLogo = `
  [lavender]      ◯[-]
  [lavender]    ensō[-]   [comment]agentic coding · Go[-]
`

// RenderSplash paints the launch screen into the chat view: logo,
// tagline, and up to 5 recent sessions with the last user message of
// each so users can pick one by content. Returns the slice of session
// ids in the order they were displayed — the host wires Alt-1..Alt-5
// (or whatever index keys are bound) to these for direct resume.
//
// store may be nil (running --ephemeral) — in that case the recent-
// sessions block is skipped silently and only the logo + bottom hint
// render. Errors listing sessions are logged inline as a quiet teal
// note, not surfaced as red, since the splash is decorative.
func RenderSplash(view *tview.TextView, store *session.Store) []string {
	fmt.Fprint(view, splashLogo)
	fmt.Fprint(view, "\n")

	var ids []string
	if store != nil {
		infos, err := session.ListRecentWithStats(store, 5)
		switch {
		case err != nil:
			fmt.Fprintf(view, "  [teal]recent sessions unavailable: %v[-]\n\n", err)
		case len(infos) > 0:
			fmt.Fprint(view, "  [comment]Recent sessions[-] [gray](Alt-1…Alt-5 to resume · Ctrl-R for full picker)[-]\n\n")
			for i, info := range infos {
				ids = append(ids, info.ID)
				meta := fmt.Sprintf("%s · %d msg · ~%s tok",
					relTime(info.UpdatedAt),
					info.MessageCount,
					compactTokenCount(info.ApproxTokens))
				header := fmt.Sprintf("[lavender]%d[-]  [mauve]%s[-]   [gray]%s[-]",
					i+1, padRight(resumeCommand(info.ID, i == 0), 28), meta)
				fmt.Fprintf(view, "    %s\n", header)
				preview := truncateOneLine(info.LastUserMessage, 72)
				if preview == "" {
					preview = "(no user messages yet)"
				}
				fmt.Fprintf(view, "       [comment]%s[-]\n", preview)
			}
			fmt.Fprint(view, "\n")
		}
	}

	fmt.Fprint(view, "  [comment]Type to start a new session · Ctrl-D to quit[-]\n\n")
	return ids
}

// resumeCommand returns the launch command that resumes the given
// session. The most recently updated session can also use the friendlier
// `--continue` form, so we prefer that for the first row.
func resumeCommand(id string, isMostRecent bool) string {
	if isMostRecent {
		return "enso --continue"
	}
	return "enso --session " + shortID(id)
}

// padRight returns s padded with spaces on the right to width w. If s is
// already wider than w, it's returned unchanged.
func padRight(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return s + strings.Repeat(" ", w-len(s))
}

// truncateOneLine returns the first line of s with leading/trailing
// whitespace stripped, truncated to at most max runes (with an ellipsis
// suffix when truncated). Used for inline message previews where multi-
// line text must collapse to a single visual row.
func truncateOneLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}
