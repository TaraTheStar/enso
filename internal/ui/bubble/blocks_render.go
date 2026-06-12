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
	"github.com/charmbracelet/x/ansi"

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
		// Live streaming: no markdown parse (partial fenced blocks
		// would render broken, and re-parsing every delta is wasteful),
		// but still hard-wrap to the terminal width so the in-flight
		// text doesn't run off the right edge before it graduates.
		// Continuation lines hang-indent under the prefix.
		lines := strings.Split(liveWrap(text, width, markdownPrefixWidth), "\n")
		var ab strings.Builder
		ab.WriteString(prefix)
		ab.WriteString(lines[0])
		pad := strings.Repeat(" ", markdownPrefixWidth)
		for _, ln := range lines[1:] {
			ab.WriteByte('\n')
			ab.WriteString(pad)
			ab.WriteString(ln)
		}
		return ab.String()

	case *blocks.Reasoning:
		bar := reasoningBar()
		// Indent every body line with the bar so multi-line thoughts
		// read as one continuous recede column. The closed form keeps
		// the same body and appends a "thought for N.Ns" footer; the
		// live form omits the footer until the block graduates.
		var out strings.Builder
		text := strings.TrimRight(v.Text, "\n")
		if text != "" {
			// Wrap to the width left of the bar so long thoughts don't
			// overflow while streaming (graduated reasoning is the same
			// recede column, just with a footer).
			wrapped := liveWrap(text, width, ansi.StringWidth(bar))
			for i, ln := range strings.Split(wrapped, "\n") {
				if i > 0 {
					out.WriteByte('\n')
				}
				out.WriteString(bar + statusStyle.Render(ln))
			}
		}
		if v.Closed {
			// Past tense: a closed block is done thinking. Live graduation
			// always records a Duration ("thought for N.Ns"); a replayed
			// block has none (Duration isn't persisted) so it shows a plain
			// "thought" rather than a misleading "thinking…".
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
			if !diffMode {
				// Wrap to the width left of the 2-cell indent so wide
				// tool output (file reads, ls) doesn't overflow. Diffs
				// are left raw — wrapping a unified diff is worse than
				// letting the terminal clip it (git behaves the same).
				body = liveWrap(body, width, 2)
			}
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

	case *blocks.Notice:
		return statusStyle.Render(v.Text)
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

// StyleDiffBlob colorizes a whole unified-diff string line-by-line with
// the same palette the in-TUI scrollback uses. Exported so the
// post-session workspace overlay can render its diff identically
// without the workspace package depending on the UI layer.
func StyleDiffBlob(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = renderDiffLine(ln)
	}
	return strings.Join(lines, "\n")
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

// liveWrap hard-wraps streaming text so a live (in-flight) block never
// runs off the right terminal edge before it graduates to the
// glamour-wrapped scrollback. limit mirrors renderMarkdown's effective
// content width (terminal width minus the block's prefix) so live and
// finalized output break at the same column. Hardwrap (not word-wrap)
// is deliberate: it guarantees no line exceeds limit even for an
// unbreakable token (a long URL/path), which is exactly the overflow
// we're preventing. No markdown parsing — cheap enough per delta.
func liveWrap(s string, width, prefixCells int) string {
	if width <= 0 {
		width = markdownDefaultWidth
	}
	limit := width - prefixCells
	if limit < 20 {
		limit = 20
	}
	return ansi.Hardwrap(s, limit, false)
}

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

// reasoningBarCached / toolBarCached hold the pre-rendered block-prefix
// strings. Both bars are pure functions of the palette, and the
// reasoning bar in particular is prepended to EVERY wrapped line of a
// live reasoning block on EVERY frame — rebuilding a lipgloss style per
// call was measurable there. applyStyles clears both on (re)theme.
var (
	reasoningBarCached string
	toolBarCached      string
)

// reasoningBar returns the dim left-bar prefix that opens reasoning lines.
// Cached at first use; invalidated by applyStyles when the palette loads.
func reasoningBar() string {
	if reasoningBarCached == "" {
		c := paletteHex("comment")
		reasoningBarCached = lipgloss.NewStyle().Foreground(c).Render("▎ ")
	}
	return reasoningBarCached
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
// Token counts and the "esc to interrupt" hint are deliberately kept
// out of this string: the interrupt affordance is rendered separately
// on the status line in View (it depends on input-buffer and pending-
// prompt state this helper doesn't see), and live token counts live
// behind /context. Turn-cancel itself works via double-Esc or Ctrl-C
// (see handleKey) — it does not quit the app.
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

// compactBarCells is the width (in glyphs) of the compaction progress
// bar. Wider than the comet spinner because it carries a real fill ratio
// — enough cells to make the gradient and the moving shimmer legible
// without dominating the status line.
const compactBarCells = 14

// compactShimmerMs is how long the shimmer head dwells on each cell. A
// touch under the spin tick (spinFrameMs=150) so the highlight reliably
// advances at least one cell between redraws.
const compactShimmerMs = 130

// compactFillGlyphs maps a cell's 0–4 quarter-fill level to a bottom-up
// braille glyph, so the leading (partially filled) cell grows row by row
// rather than snapping full. Index 0 is unused (empty cells render a
// faint track instead); 1–4 are ⣀ ⣤ ⣶ ⣿.
var compactFillGlyphs = [5]string{"⣀", "⣀", "⣤", "⣶", "⣿"}

// compactingIndicator renders the animated braille progress bar shown on
// the status line while a tier-2 compaction summary streams. Three
// things make it feel alive:
//   - the fill tracks pct (bottom-up, quarter-cell granularity on the
//     leading glyph);
//   - a bright shimmer cell sweeps left→right across the bar on
//     wall-clock time (so it animates even when token deltas are sparse,
//     and — like spinFrame — every concurrent render agrees on the
//     frame, the spin tick merely forcing the redraw cadence);
//   - the fill runs a teal→lavender→mauve gradient pulled from the live
//     palette, tying it to enso's chrome rather than a generic widget.
//
// The pct is the agent's soft estimate (see compactProgressTarget); the
// caller guarantees it's only shown between turns, so this owns the line.
func compactingIndicator(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	// Fill expressed in quarter-cell sub-units for the partial leading glyph.
	totalQuarters := pct * compactBarCells * 4 / 100
	// Shimmer head drifts from just before the bar to just past it, so
	// each sweep reads as a discrete pass with a gap rather than an
	// unbroken ribbon.
	shimmer := int(time.Now().UnixMilli()/compactShimmerMs)%(compactBarCells+4) - 2

	var b strings.Builder
	for i := 0; i < compactBarCells; i++ {
		q := totalQuarters - i*4
		if q <= 0 {
			// Unfilled track: a faint floor of dots so the bar's full
			// width is always visible and the fill reads as rising into it.
			b.WriteString(spinDimStyle.Render(compactFillGlyphs[0]))
			continue
		}
		level := 4
		if q < 4 {
			level = q
		}
		col := compactCellColor(i, shimmer)
		b.WriteString(lipgloss.NewStyle().Foreground(col).Render(compactFillGlyphs[level]))
	}
	return b.String() + " " + statusStyle.Render(fmt.Sprintf("compacting… %d%%", pct))
}

// compactCellColor resolves a filled cell's colour: the base
// teal→lavender→mauve gradient at this cell's position, brightened
// toward white when the shimmer head is on (or just past) it.
func compactCellColor(i, shimmer int) color.Color {
	t := 0.0
	if compactBarCells > 1 {
		t = float64(i) / float64(compactBarCells-1)
	}
	r, g, bl := compactGradient(t)
	if d := i - shimmer; d >= 0 && d <= 1 {
		boost := 0.55
		if d == 1 {
			boost = 0.25 // soft trailing edge behind the head
		}
		r = lerpComponent(r, 255, boost)
		g = lerpComponent(g, 255, boost)
		bl = lerpComponent(bl, 255, boost)
	}
	return lipgloss.Color(fmt.Sprintf("#%02x%02x%02x", clampComponent(r), clampComponent(g), clampComponent(bl)))
}

// compactGradient interpolates teal→lavender→mauve for t in [0,1],
// reading the live palette so user themes flow through.
func compactGradient(t float64) (r, g, b float64) {
	teal := compactPaletteRGB("teal")
	lav := compactPaletteRGB("lavender")
	mauve := compactPaletteRGB("mauve")
	if t < 0.5 {
		u := t / 0.5
		return lerpComponent(teal[0], lav[0], u), lerpComponent(teal[1], lav[1], u), lerpComponent(teal[2], lav[2], u)
	}
	u := (t - 0.5) / 0.5
	return lerpComponent(lav[0], mauve[0], u), lerpComponent(lav[1], mauve[1], u), lerpComponent(lav[2], mauve[2], u)
}

func compactPaletteRGB(name string) [3]float64 {
	if c, ok := currentPalette[name]; ok {
		return [3]float64{float64(c.R & 0xff), float64(c.G & 0xff), float64(c.B & 0xff)}
	}
	return [3]float64{200, 200, 200}
}

func lerpComponent(a, b, t float64) float64 { return a + (b-a)*t }

func clampComponent(v float64) int {
	switch {
	case v < 0:
		return 0
	case v > 255:
		return 255
	default:
		return int(v)
	}
}

// ctxGaugeCells is the width of the inline context-window gauge in the
// status line. Compact on purpose — it shares the line with the model
// name, Σ cost, and the interrupt hint.
const ctxGaugeCells = 10

// contextGauge renders the bounded context-window gauge: a fraction of
// the REAL window (clamped to full — it can't exceed the model's true
// ceiling), with the compaction budget drawn as a marker. Cells filled
// past the budget are amber (the over-budget / slow-decode zone); cells
// within budget are teal. When no decoupled compaction_budget is
// configured (budget <= 0) the marker falls at the legacy 80% line so
// existing setups still get a warning band.
func contextGauge(used, budget, window int) string {
	if window <= 0 {
		return ""
	}
	frac := float64(used) / float64(window)
	if frac > 1 {
		frac = 1
	}
	if frac < 0 {
		frac = 0
	}
	fill := int(frac*ctxGaugeCells + 0.5)

	markerCell := -1
	switch {
	case budget > 0 && budget < window:
		markerCell = int(float64(budget)/float64(window)*ctxGaugeCells + 0.5)
	case budget <= 0:
		markerCell = ctxGaugeCells * 8 / 10 // legacy 80% warn line
	}
	if markerCell >= ctxGaugeCells {
		markerCell = ctxGaugeCells - 1
	}

	fillStyle := lipgloss.NewStyle().Foreground(paletteHex("teal"))
	markerStyle := lipgloss.NewStyle().Foreground(paletteHex("comment"))

	var b strings.Builder
	b.WriteString(statusStyle.Render("▕"))
	for i := 0; i < ctxGaugeCells; i++ {
		filled := i < fill
		past := markerCell >= 0 && i >= markerCell
		switch {
		case filled && past:
			b.WriteString(noticeStyle.Render("█")) // over budget → amber
		case filled:
			b.WriteString(fillStyle.Render("█"))
		case i == markerCell:
			b.WriteString(markerStyle.Render("┊")) // budget marker on the empty track
		default:
			b.WriteString(statusStyle.Render("·"))
		}
	}
	b.WriteString(statusStyle.Render("▏"))
	return b.String()
}

// toolBar returns the teal arrow prefix that opens a tool call.
// Cached like reasoningBar.
func toolBar() string {
	if toolBarCached == "" {
		c := paletteHex("teal")
		toolBarCached = lipgloss.NewStyle().Foreground(c).Bold(true).Render("▌ ⏵ ")
	}
	return toolBarCached
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
