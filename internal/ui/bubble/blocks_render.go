// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/TaraTheStar/enso/internal/ui/blocks"
	"github.com/TaraTheStar/enso/internal/ui/theme"
)

// renderBlock returns the Lipgloss-styled multi-line string for a
// block. Used both as the live-region rendering of the in-flight block
// and as the scrollback graduation payload (tea.Println'd by the
// model). Returns "" for blocks that have no display form (rare; mostly
// empty assistant blocks from tool-call-only turns).
func renderBlock(b blocks.Block) string {
	switch v := b.(type) {
	case *blocks.User:
		text := strings.TrimRight(v.Text, "\n")
		if text == "" {
			return ""
		}
		return userStyle.Render("you") + "  " + text

	case *blocks.Assistant:
		text := strings.TrimRight(v.Text, "\n")
		if text == "" {
			return ""
		}
		return asstStyle.Render("enso") + " " + text

	case *blocks.Reasoning:
		bar := reasoningBar()
		if v.Closed {
			label := "thinking…"
			if v.Duration > 0 {
				label = fmt.Sprintf("thought for %s", fmtDuration(v.Duration))
			}
			return bar + statusStyle.Render(label)
		}
		// Live reasoning: indent each line with the bar so multi-line
		// thoughts read as one continuous recede column.
		lines := strings.Split(strings.TrimRight(v.Text, "\n"), "\n")
		var out strings.Builder
		for i, ln := range lines {
			if i > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(bar + statusStyle.Render(ln))
		}
		return out.String()

	case *blocks.Tool:
		var out strings.Builder
		out.WriteString(toolBar())
		out.WriteString(v.Call)
		if badge := toolBadge(v); badge != "" {
			out.WriteString(statusStyle.Render("  " + badge))
		}
		if v.Output != "" {
			body := strings.TrimRight(v.Output, "\n")
			indented := "  " + strings.ReplaceAll(body, "\n", "\n  ")
			out.WriteByte('\n')
			out.WriteString(statusStyle.Render(indented))
		}
		return out.String()

	case *blocks.Error:
		return errorStyle.Render("✘ " + v.Msg)

	case *blocks.Cancelled:
		return statusStyle.Render("(cancelled)")

	case *blocks.Compacted:
		return statusStyle.Render(fmt.Sprintf("(compacted: %s → %s)", fmtTokens(v.Before), fmtTokens(v.After)))

	case *blocks.InputDiscarded:
		return noticeStyle.Render(fmt.Sprintf("(discarded %d queued message%s)", v.Count, plural(v.Count)))
	}
	return ""
}

// reasoningBar returns the dim left-bar prefix that opens reasoning lines.
// Cached at first use rather than recomputed each call.
func reasoningBar() string {
	c := paletteHex("comment")
	return lipgloss.NewStyle().Foreground(c).Render("▎ ")
}

// toolBar returns the teal arrow prefix that opens a tool call.
func toolBar() string {
	c := paletteHex("teal")
	return lipgloss.NewStyle().Foreground(c).Bold(true).Render("▌ ⏵ ")
}

// paletteHex resolves a palette colour to a lipgloss.Color. Used by
// per-block styling that doesn't fit cleanly into the package-level
// styles in styles.go.
func paletteHex(name string) lipgloss.Color {
	pal := theme.Default()
	if c, ok := pal[name]; ok {
		return lipgloss.Color(c.Hex())
	}
	return lipgloss.Color("")
}

// toolBadge renders the short trailing duration segment for tool blocks
// long enough to warrant one. Mirrors tui's two thresholds: live blocks
// get a badge after 2s, completed blocks get a badge if they ran
// longer than 1s.
func toolBadge(b *blocks.Tool) string {
	const liveThreshold = 2 * time.Second
	const finalThreshold = time.Second
	switch {
	case b.Running() && b.Elapsed() >= liveThreshold:
		return "· running " + fmtDuration(b.Elapsed())
	case !b.Running() && b.Duration >= finalThreshold:
		return "· " + fmtDuration(b.Duration)
	}
	return ""
}

// fmtDuration renders short human-readable durations. Sub-minute values
// keep one decimal of seconds; minute-plus values drop the decimal.
func fmtDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	mins := int(d.Minutes())
	secs := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", mins, secs)
}

// fmtTokens renders a token count with `k` suffixing for ≥1000.
func fmtTokens(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", n/1000)
	}
	return fmt.Sprintf("%d", n)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
