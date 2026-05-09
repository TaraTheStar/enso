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
		// Spacing chosen so "you" + 2 spaces and "enso" + 1 space both
		// land the chevron at the same column — message bodies align.
		// The chevron echoes the input prompt's "›" so a rendered user
		// message reads as a continuation of what was just typed.
		return userStyle.Render("you") + "  " + statusStyle.Render("›") + " " + text

	case *blocks.Assistant:
		text := strings.TrimRight(v.Text, "\n")
		if text == "" {
			return ""
		}
		return asstStyle.Render("enso") + " " + statusStyle.Render("›") + " " + renderAssistantBody(text)

	case *blocks.Reasoning:
		bar := reasoningBar()
		// Indent every body line with the bar so multi-line thoughts
		// read as one continuous recede column. The closed form keeps
		// the same body and appends a "thought for N.Ns" footer; the
		// live form omits the footer until the block graduates.
		var out strings.Builder
		text := strings.TrimRight(v.Text, "\n")
		if text != "" {
			lines := strings.Split(text, "\n")
			for i, ln := range lines {
				if i > 0 {
					out.WriteByte('\n')
				}
				out.WriteString(bar + statusStyle.Render(ln))
			}
		}
		if v.Closed {
			label := "thinking…"
			if v.Duration > 0 {
				label = fmt.Sprintf("thought for %s", fmtDuration(v.Duration))
			}
			if out.Len() > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(bar + statusStyle.Render(label))
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
			out.WriteByte('\n')
			// lipgloss.Render pads every line of a multi-line input to
			// the width of the longest line, which turns wide tool
			// output (file reads, ls listings) into a sea of trailing
			// whitespace. Rendering each line on its own avoids that.
			//
			// Diff-shaped output gets per-line semantic colouring so
			// edit/write tool results are scannable; everything else
			// stays in the recede status colour.
			diffMode := looksLikeDiff(body)
			for i, ln := range strings.Split(body, "\n") {
				if i > 0 {
					out.WriteByte('\n')
				}
				out.WriteString("  ")
				if diffMode {
					out.WriteString(renderDiffLine(ln))
				} else {
					out.WriteString(statusStyle.Render(ln))
				}
			}
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

// looksLikeDiff reports whether tool output appears to be a unified
// diff. The "@@ -" hunk header is a strong, low-false-positive signal;
// generic +/- prefixes alone aren't, since plenty of regular tool
// output (lists, ascii art, command flags) starts with those.
func looksLikeDiff(s string) bool {
	return strings.Contains(s, "\n@@ ") || strings.HasPrefix(s, "@@ ")
}

// renderDiffLine returns one diff line styled by its role: + sage,
// - red, @@ teal, --- / +++ bold gray, everything else dim. The
// caller handles indentation; this only colors the line content.
func renderDiffLine(line string) string {
	switch {
	case strings.HasPrefix(line, "@@"):
		return diffHunkStyle.Render(line)
	case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		return diffFileStyle.Render(line)
	case strings.HasPrefix(line, "+"):
		return diffAddStyle.Render(line)
	case strings.HasPrefix(line, "-"):
		return diffDelStyle.Render(line)
	default:
		return statusStyle.Render(line)
	}
}

// renderAssistantBody walks the assistant's response and visually
// brackets fenced ```…``` code blocks with a left bar so code reads
// as a distinct region from prose. The fence lines themselves are
// suppressed — the bar is enough of a marker, and dropping the
// triple-backtick noise reads cleaner. Body lines inside fences keep
// the terminal's default colour so code stays legible; only the bar
// is styled.
//
// No syntax highlighting yet — that lands when (if) we adopt glamour
// + chroma. Inline `code` spans are also untouched for the same
// reason: the simple-detection version is unreliable, and the proper
// fix is glamour.
func renderAssistantBody(text string) string {
	if !strings.Contains(text, "```") {
		return text
	}
	var rendered []string
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			// Suppress the fence line itself; the bar is sufficient
			// marking and dropping the triple-backtick noise reads
			// cleaner.
			continue
		}
		if inFence {
			rendered = append(rendered, codeBarStyle.Render("│ ")+line)
		} else {
			rendered = append(rendered, line)
		}
	}
	return strings.Join(rendered, "\n")
}

// reasoningBar returns the dim left-bar prefix that opens reasoning lines.
// Cached at first use rather than recomputed each call.
func reasoningBar() string {
	c := paletteHex("comment")
	return lipgloss.NewStyle().Foreground(c).Render("▎ ")
}

// cometBandSize is the width (in cells) of the comet-trail spinner.
// Five cells gives the head room to traverse without the trail
// crowding it; smaller would feel stuttery, wider would steal too
// much horizontal space from the rest of the status line.
const cometBandSize = 5

// cometFrame describes one frame of the comet-trail spinner: the head
// and trail are cell indices into a cometBandSize-wide band, with -1
// meaning "off-band". Seven frames total — five carrying the comet
// across, one with only the trail remaining, one fully empty so each
// pass reads as a discrete pulse rather than an unbroken ribbon.
type cometFrame struct{ head, trail int }

var cometFrames = []cometFrame{
	{0, -1},
	{1, 0},
	{2, 1},
	{3, 2},
	{4, 3},
	{-1, 4},
	{-1, -1},
}

// spinFrame returns the current pre-styled comet-trail frame derived
// from wall clock time. Time-based frame selection means every
// concurrent render lands on the same frame, keeping the animation
// coherent across redraws driven by both spinTickMsg and the regular
// event-driven re-renders.
//
// Design note: this is intentionally neither claude-code's pulsing
// single-glyph sigil (✻ ✦ ✱ ✷) nor crush's 15-cell scrolling band of
// random alphanumerics. The middle ground — a small fixed-width band
// with one bright drifting cell — keeps the "moving texture" feel of
// crush without the visual noise, and the two-tone palette ties it
// into the assistant accent rather than demanding its own colour.
func spinFrame() string {
	f := cometFrames[(time.Now().UnixMilli()/spinFrameMs)%int64(len(cometFrames))]
	var b strings.Builder
	for i := 0; i < cometBandSize; i++ {
		switch i {
		case f.head:
			b.WriteString(spinHeadStyle.Render("●"))
		case f.trail:
			b.WriteString(spinTrailStyle.Render("•"))
		default:
			b.WriteString(spinDimStyle.Render("∙"))
		}
	}
	return b.String()
}

// waitingIndicator is the bottom-line status shown after the user has
// submitted a message but before any block has gone live — i.e., we're
// waiting on the agent to start streaming. Cold starts, model switches,
// and slow reasoning models can stretch this to several seconds; the
// elapsed counter reassures the user that work is actually in flight.
//
// The returned string is pre-styled (the comet uses its own per-cell
// colours) so the caller must NOT re-wrap it in statusStyle.
func waitingIndicator(since time.Time) string {
	elapsed := time.Duration(0)
	if !since.IsZero() {
		elapsed = time.Since(since)
	}
	return spinFrame() + " " + statusStyle.Render("waiting… ("+fmtDuration(elapsed)+")")
}

// liveIndicator returns the status line shown immediately above the
// input prompt while a block is live. Returns "" when there's nothing
// in flight, in which case the caller falls back to the model name.
//
// The format mirrors claude-code's pattern: spinner + label + elapsed.
// We deliberately don't include token counts or "esc to interrupt" —
// the latter is misleading until turn-cancel is wired up (today only
// ctrl+c works, and it quits the whole app).
func liveIndicator(b blocks.Block, started time.Time) string {
	if b == nil {
		return ""
	}
	var label string
	switch v := b.(type) {
	case *blocks.Reasoning:
		label = "thinking…"
	case *blocks.Assistant:
		label = "responding…"
	case *blocks.Tool:
		// Replay paths can leave a Tool block "live" with no started
		// timestamp; treat those as not actually running.
		if !v.Running() {
			return ""
		}
		name := v.Name
		if name == "" {
			name = "tool"
		}
		label = "running " + name
	default:
		return ""
	}
	elapsed := time.Duration(0)
	if !started.IsZero() {
		elapsed = time.Since(started)
	}
	return spinFrame() + " " + statusStyle.Render(label+" ("+fmtDuration(elapsed)+")")
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
