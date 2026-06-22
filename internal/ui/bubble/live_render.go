// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/TaraTheStar/enso/internal/ui/blocks"
)

// liveRenderCache memoizes the expensive part of rendering the
// in-flight (live) block: hard-wrapping and per-line styling of text
// that has already streamed. View() re-renders the live block on every
// frame — the 150ms spinner tick AND every 16ms batch of streaming
// deltas — and the plain renderBlock re-wraps (ansi.Hardwrap, O(text))
// and re-styles the FULL accumulated text each time, making a long
// stream O(n²) overall (a local model streaming 24K tokens is ~96KB).
// Streaming blocks only ever append (conversation.HandleEvent does
// `Text += s`), so everything up to the last completed raw line is
// immutable: its wrapped/styled form is cached here and only the
// trailing incomplete line is re-wrapped/re-styled per frame.
//
// Correctness: the output is byte-identical to renderBlock(b, width,
// false). Folding complete raw lines one batch at a time is equivalent
// to wrapping the whole text because ansi.Hardwrap resets its column
// state (curWidth/forceNewline) at every '\n'; verified across
// incremental appends and width changes by TestLiveRenderMatchesRenderBlock.
// Graduated (finalized) rendering never goes through this cache, so the
// scrollback form is untouched.
//
// The cache is owned by the model and only used from the Bubble Tea
// event-loop goroutine (View); no locking. Width changes and live-block
// transitions invalidate it wholesale.
type liveRenderCache struct {
	block    blocks.Block    // identity of the cached live block
	width    int             // wrap width the cache was built for
	consumed int             // bytes of the block's (trimmed) text folded into body
	lines    int             // wrapped lines accumulated in body
	body     strings.Builder // rendered stable prefix (continuation separators included, no first-line prefix)
}

func (c *liveRenderCache) reset(b blocks.Block, width int) {
	c.block = b
	c.width = width
	c.consumed = 0
	c.lines = 0
	c.body.Reset()
}

// ensure (re)initializes the cache when it cannot extend the current
// state: different block, different width, or text shrank (can't happen
// on the append-only stream; defensive).
func (c *liveRenderCache) ensure(b blocks.Block, width, textLen int) {
	if c.block != b || c.width != width || c.consumed > textLen {
		c.reset(b, width)
	}
}

// render returns the live-region rendering of b, identical to
// renderBlock(b, width, false) but with the stable prefix of streaming
// Assistant/Reasoning blocks served from the cache. Every other block
// kind falls through to renderBlock (Tool output arrives in bounded
// progress chunks and graduates promptly; nothing else streams).
func (c *liveRenderCache) render(b blocks.Block, width int) string {
	switch v := b.(type) {
	case *blocks.Assistant:
		return c.renderAssistant(v, width)
	case *blocks.Reasoning:
		return c.renderReasoning(v, width)
	default:
		return renderBlock(b, width, false)
	}
}

// fold appends the newly-completed raw lines (text up to and including
// the last '\n', past what was already consumed) to the cached body.
// style renders one wrapped line; sep is written before every wrapped
// line except the very first of the block. Returns the byte offset of
// the start of the still-streaming tail.
func (c *liveRenderCache) fold(text string, prefixCells int, sep string, style func(string) string) int {
	stable := strings.LastIndexByte(text, '\n') + 1
	if stable > c.consumed {
		for _, raw := range strings.Split(strings.TrimSuffix(text[c.consumed:stable], "\n"), "\n") {
			for _, ln := range strings.Split(liveWrap(raw, c.width, prefixCells), "\n") {
				if c.lines > 0 {
					c.body.WriteString(sep)
				}
				c.body.WriteString(style(ln))
				c.lines++
			}
		}
		c.consumed = stable
	}
	return stable
}

// renderAssistant mirrors renderBlock's live Assistant arm (raw text,
// hard-wrapped, hang-indented under the "enso › " prefix).
func (c *liveRenderCache) renderAssistant(v *blocks.Assistant, width int) string {
	text := strings.TrimRight(v.Text, "\n")
	if text == "" {
		return ""
	}
	// Mirror renderBlock's live arm: fold LaTeX to Unicode before wrapping.
	// delatexStream transforms only complete lines in full (immutable once
	// their newline has streamed) and holds a trailing partial command raw,
	// so the cache's stable-prefix invariant still holds.
	text = delatexStream(text)
	c.ensure(v, width, len(text))
	pad := strings.Repeat(" ", markdownPrefixWidth)
	stable := c.fold(text, markdownPrefixWidth, "\n"+pad, func(ln string) string { return ln })

	var ab strings.Builder
	ab.WriteString(asstStyle.Render("enso") + " " + statusStyle.Render("›") + " ")
	ab.WriteString(c.body.String())
	// Tail: the incomplete last raw line (never empty — text is
	// TrimRight'ed, so it can't end in '\n'). Re-wrapped every frame;
	// bounded by one raw line, not the whole stream.
	n := c.lines
	for _, ln := range strings.Split(liveWrap(text[stable:], width, markdownPrefixWidth), "\n") {
		if n > 0 {
			ab.WriteByte('\n')
			ab.WriteString(pad)
		}
		ab.WriteString(ln)
		n++
	}
	return ab.String()
}

// renderReasoning mirrors renderBlock's Reasoning arm (recede column
// behind the bar, optional "thought…" footer when closed).
func (c *liveRenderCache) renderReasoning(v *blocks.Reasoning, width int) string {
	bar := reasoningBar()
	text := strings.TrimRight(v.Text, "\n")
	var out strings.Builder
	if text != "" {
		c.ensure(v, width, len(text))
		barCells := ansi.StringWidth(bar)
		stable := c.fold(text, barCells, "\n", func(ln string) string { return bar + statusStyle.Render(ln) })
		out.WriteString(c.body.String())
		n := c.lines
		for _, ln := range strings.Split(liveWrap(text[stable:], width, barCells), "\n") {
			if n > 0 {
				out.WriteByte('\n')
			}
			out.WriteString(bar + statusStyle.Render(ln))
			n++
		}
	}
	if v.Closed {
		label := "thought"
		if v.Duration > 0 {
			label = fmt.Sprintf("thought for %s", fmtDuration(v.Duration))
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(bar + statusStyle.Render(label))
	}
	return out.String()
}
