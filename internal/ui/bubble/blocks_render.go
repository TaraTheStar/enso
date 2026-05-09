// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"fmt"
	"image/color"
	"regexp"
	"strings"
	"sync"
	"time"

	"charm.land/glamour/v2"
	"charm.land/lipgloss/v2"

	"github.com/TaraTheStar/enso/internal/ui/blocks"
	"github.com/TaraTheStar/enso/internal/ui/theme"
)

// mdTrailingPadRE matches glamour's per-line trailing padding in
// three forms, repeated at end of line: (1) plain whitespace; (2)
// styled "colour, optional whitespace, reset" cells (block-quote
// fills); (3) zero-width "colour, reset" sentinels glamour emits at
// the very end of styled blocks. All three are visually invisible
// but bloat scrollback and copy-paste selections — the alternation
// `\x1b[<codes>m\s*\x1b[0m` is intentionally narrow (only matches
// styled regions whose interior is *only* whitespace), so we never
// strip a colour-wrapped piece of real content.
var mdTrailingPadRE = regexp.MustCompile(`(?:\x1b\[[0-9;]*m[ \t]*\x1b\[0?m|[ \t]+)+$`)

// renderBlock returns the Lipgloss-styled multi-line string for a
// block. Used both as the live-region rendering of the in-flight block
// and as the scrollback graduation payload (tea.Println'd by the
// model). Returns "" for blocks that have no display form (rare; mostly
// empty assistant blocks from tool-call-only turns).
//
// width is the terminal width used for word-wrapping markdown content
// (0 falls back to a sensible default). finalized=true switches the
// Assistant render path from raw streaming text to glamour-rendered
// markdown with syntax-highlighted code blocks; finalized=false keeps
// raw text so partially-streamed fences and tables don't render as
// broken markdown.
func renderBlock(b blocks.Block, width int, finalized bool) string {
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
		prefix := asstStyle.Render("enso") + " " + statusStyle.Render("›") + " "
		if finalized {
			return prefix + renderMarkdown(text, width)
		}
		// Live streaming: raw text. Partial fenced blocks would render
		// as broken markdown, and re-parsing on every delta would burn
		// CPU for no visual gain.
		return prefix + text

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

// markdownDefaultWidth is the wrap width used when the caller has no
// terminal width available (replay scripts, tests, etc.). Wide enough
// to look natural, narrow enough to not overrun an 80-col terminal.
const markdownDefaultWidth = 100

// markdownPrefixWidth is how many cells the "enso › " prefix consumes
// on the first line of an assistant block. Glamour's word wrap doesn't
// know about content rendered before its output, so we shrink the
// reported terminal width by this much to keep the *first* wrapped
// line from running past the terminal edge.
const markdownPrefixWidth = 7

// renderMarkdown turns assistant markdown into glamour-rendered output:
// fenced ```lang``` blocks pick up syntax highlighting via chroma,
// inline code, bold/italic, lists and headings render with theme
// colours. Only called for *finalized* assistant blocks (graduating to
// scrollback) — live streaming keeps raw text so partial fences and
// tables don't render as broken markdown.
//
// Errors in glamour fall back to raw text rather than swallowing the
// content; a render error shouldn't lose the assistant's reply.
func renderMarkdown(text string, width int) string {
	if width <= 0 {
		width = markdownDefaultWidth
	}
	wrap := width - markdownPrefixWidth
	if wrap < 20 {
		wrap = 20
	}
	r, err := markdownRenderer(wrap)
	if err != nil {
		return text
	}
	out, err := r.Render(text)
	if err != nil {
		return text
	}
	// Strip glamour's per-line right-padding (see mdTrailingPadRE) so
	// scrollback selection isn't dragged by invisible trailing spaces.
	// Then trim leading/trailing blank lines that glamour wraps the
	// document in — the leading blank would push the first line below
	// the "enso › " prefix, the trailing one doubles up with the
	// spacing the View already inserts.
	lines := strings.Split(out, "\n")
	for i, ln := range lines {
		lines[i] = mdTrailingPadRE.ReplaceAllString(ln, "")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// markdownRenderer caches one glamour renderer per wrap width. Glamour
// renderer construction parses the style and instantiates chroma, so
// building one per call is wasteful — every assistant graduation
// would redo that work. The cache is keyed by width since wrap is the
// only per-call variable; the style is held constant within a theme
// generation. invalidateMarkdownRenderers clears the cache when the
// palette is reloaded so theme overrides take effect on the very next
// render.
var (
	mdRenderers   = map[int]*glamour.TermRenderer{}
	mdRenderersMu sync.Mutex
)

func markdownRenderer(wrap int) (*glamour.TermRenderer, error) {
	mdRenderersMu.Lock()
	defer mdRenderersMu.Unlock()
	if r, ok := mdRenderers[wrap]; ok {
		return r, nil
	}
	pal := currentPalette
	if pal == nil {
		pal = theme.Default()
	}
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(buildMarkdownTheme(pal)),
		glamour.WithWordWrap(wrap),
	)
	if err != nil {
		return nil, err
	}
	mdRenderers[wrap] = r
	return r, nil
}

// invalidateMarkdownRenderers drops the renderer cache so the next
// render picks up an updated theme palette. Called from applyStyles
// whenever the palette is (re)loaded.
func invalidateMarkdownRenderers() {
	mdRenderersMu.Lock()
	defer mdRenderersMu.Unlock()
	mdRenderers = map[int]*glamour.TermRenderer{}
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

// paletteHex resolves a palette colour to a color.Color usable by any
// lipgloss style method. Used by per-block styling that doesn't fit
// cleanly into the package-level styles in styles.go.
func paletteHex(name string) color.Color {
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
