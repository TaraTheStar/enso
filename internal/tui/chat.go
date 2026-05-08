// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
)

// Visual conventions:
//
//   user:        [ USER ] pill  (mauve reverse-video) over plain message body
//   assistant:   [ ENSŌ ] pill (lavender reverse-video) over plain body
//   tool call:   ▌ ⏵ name(args)  (teal bar + glyph; default text)
//   reasoning:   ▎ thinking…  (muted-lavender bar; same color streamed live)
//                ▎ thought for N.Ns  (footer when closed)
//   error:       ✘ msg  (rose prefix)
//   cancelled / compacted: parenthetical in teal
//
// Two distinct purples deliberately split user (mauve, pinker) from
// assistant (lavender, bluer/hero) so the conversation reads as two
// voices, not "more purple". Pills are emitted as
// `[black:<color>:b] LABEL [-:-:-]` (tview tag syntax is `[fg:bg:attr]`);
// body text follows on the next line so click-and-drag selection still
// copies clean prose. No borders, no backgrounds on the chat container.
//
// The display also keeps a parallel `[]chatBlock` so toggling reasoning
// visibility (Ctrl-T) can rebuild the view from scratch with collapsed
// thinking blocks.

const (
	pillUser     = "[black:mauve:b] USER [-:-:-]"
	pillAsst     = "[black:lavender:b] ENSŌ [-:-:-]"
	barTool      = "[teal]▌ ⏵ "
	barToolEnd   = "[-]"
	barReasoning = "[comment]▎ "
	barReasonEnd = "[-]"
	errPrefix    = "[red]✘[-] "
)

// streamMode tracks what kind of content is currently being streamed so the
// renderer can close out one block before opening another.
type streamMode int

const (
	streamNone streamMode = iota
	streamAssistant
	streamReasoning
)

// Tags some local models emit inline to delimit their chain-of-thought.
// Honoured regardless of which delta channel they arrive on, since
// llama.cpp's reasoning-content/content split is unreliable across templates.
const (
	thinkOpen  = "<think>"
	thinkClose = "</think>"
)

// chatBlock is the persisted form of one rendered region. Streaming events
// mutate the most recent block; toggle / replay walks the slice and renders.
type chatBlock interface {
	render(view *tview.TextView, showThinking bool)
}

type userBlock struct{ text string }
type assistantBlock struct{ text string }

// assistantHeaderBlock is the once-per-turn pill that introduces the
// assistant's response. Inserted before any reasoning, tool call, or
// content for the turn so all three sit visually beneath the same
// header. Carries no state — render just paints the pill.
type assistantHeaderBlock struct{}
type toolBlock struct {
	id        string
	name      string        // tool name (e.g. "bash", "read", "lsp_hover") — drives the untrusted-content marker
	call      string        // formatted "name(arg=val, ...)" for display
	output    string        // accumulated stdout/stderr from progress events
	startedAt time.Time     // wall-clock time the call began (zero on replay/legacy blocks — no badge then)
	duration  time.Duration // zero while running; final wall-clock duration once End fires
}

// liveBadgeThreshold is how long a tool call must run before its
// elapsed-time badge appears in the chat. Most read/grep/glob calls
// finish in <500ms; surfacing "0s" / "1s" on those would flicker
// without conveying anything useful — the threshold means the badge
// appears only when the wait actually starts to matter.
const liveBadgeThreshold = 2 * time.Second

// finalBadgeThreshold is the lower bound for showing a persistent
// duration on completed tool blocks. Sub-second calls stay un-badged
// so scrolled-back history stays clean.
const finalBadgeThreshold = time.Second

// running reports whether this block represents an in-flight tool call.
// Replay/resume paths leave startedAt zero, so they never look "running"
// even though duration is also zero.
func (b *toolBlock) running() bool {
	return !b.startedAt.IsZero() && b.duration == 0
}

// elapsed is the duration to display in the badge: live time-since for
// running calls, the recorded final duration for completed ones.
func (b *toolBlock) elapsed() time.Duration {
	if b.startedAt.IsZero() {
		return 0
	}
	if b.duration > 0 {
		return b.duration
	}
	return time.Since(b.startedAt)
}

type errorBlock struct{ msg string }
type cancelledBlock struct{}
type inputDiscardedBlock struct{ count int }
type compactedBlock struct {
	before int
	after  int
}
type reasoningBlock struct {
	text     string
	started  time.Time
	duration time.Duration // zero while still open
	closed   bool
}

func (b *userBlock) render(v *tview.TextView, _ bool) {
	fmt.Fprintf(v, "%s\n%s\n\n", pillUser, b.text)
}

func (b *assistantHeaderBlock) render(v *tview.TextView, _ bool) {
	fmt.Fprintf(v, "%s\n", pillAsst)
}

func (b *assistantBlock) render(v *tview.TextView, _ bool) {
	if b.text == "" {
		return
	}
	// Pill is emitted by the preceding assistantHeaderBlock; here we
	// only paint the body so reasoning + tool calls + content all share
	// one pill at the top of the turn.
	fmt.Fprintf(v, "%s\n\n", b.text)
}

func (b *toolBlock) render(v *tview.TextView, _ bool) {
	marker := ""
	if isUntrustedContentTool(b.name) {
		marker = untrustedMarker
	}
	fmt.Fprintf(v, "%s%s%s%s%s\n", barTool, marker, b.call, barToolEnd, toolBlockBadge(b))
	if b.output != "" {
		// Indent each output line by two spaces and dim it. Trailing
		// newline normalised so we always produce a single blank-line gap
		// before whatever comes next.
		out := strings.TrimRight(b.output, "\n")
		out = strings.ReplaceAll(out, "\n", "\n  ")
		fmt.Fprintf(v, "[gray]  %s[-]\n", out)
	}
	fmt.Fprint(v, "\n")
}

func (b *errorBlock) render(v *tview.TextView, _ bool) {
	fmt.Fprintf(v, "%s%s\n\n", errPrefix, b.msg)
}

func (b *cancelledBlock) render(v *tview.TextView, _ bool) {
	fmt.Fprint(v, "[teal](cancelled)[-]\n\n")
}

func (b *inputDiscardedBlock) render(v *tview.TextView, _ bool) {
	noun := "messages"
	if b.count == 1 {
		noun = "message"
	}
	fmt.Fprintf(v, "[teal](%d %s discarded after cancel)[-]\n\n", b.count, noun)
}

func (b *compactedBlock) render(v *tview.TextView, _ bool) {
	if b.before > 0 && b.after > 0 {
		fmt.Fprintf(v, "[teal](context compacted: %s → %s)[-]\n\n",
			compactTokenCount(b.before), compactTokenCount(b.after))
		return
	}
	fmt.Fprint(v, "[teal](context compacted)[-]\n\n")
}

// compactionTokens extracts before/after counts from an EventCompacted
// payload. Local in-process events carry ints; daemon-attached events
// arrive via JSON and surface as float64 — handle both.
func compactionTokens(payload any) (before, after int) {
	m, ok := payload.(map[string]any)
	if !ok {
		return 0, 0
	}
	return tokenField(m["before_tokens"]), tokenField(m["after_tokens"])
}

func tokenField(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func (b *reasoningBlock) render(v *tview.TextView, showThinking bool) {
	if !showThinking && b.closed {
		lines := countLines(b.text)
		fmt.Fprintf(v, "[comment]▎ thought for %s · %d line%s (Ctrl-T to expand)[-]\n\n",
			fmtDuration(b.duration), lines, plural(lines))
		return
	}
	body := strings.ReplaceAll(b.text, "\n", "\n"+barReasoning)
	if body != "" {
		fmt.Fprintf(v, "%s%s%s\n", barReasoning, body, barReasonEnd)
	}
	if b.closed {
		fmt.Fprintf(v, "[comment]▎ thought for %s[-]\n\n", fmtDuration(b.duration))
	}
}

// ChatDisplay renders the conversation in a TextView, appending each event
// and maintaining a parallel block model for re-render on toggle.
type ChatDisplay struct {
	view         *tview.TextView
	modelID      string
	stream       streamMode
	showThinking bool

	// tagState reflects what an inline `<think>` / `</think>` marker last
	// said, and overrides the source channel when set:
	//   streamReasoning  = inside a <think> block
	//   streamAssistant  = saw </think>, post-think text is the answer
	//   streamNone       = no tag seen for this turn; honour source channel
	// Reset at user-message and AssistantDone boundaries.
	tagState streamMode

	// pending holds trailing bytes from a delta that could be the start of
	// a `<think>` / `</think>` tag — prepended onto the next delta so a tag
	// split across SSE chunks is still recognised. Flushed as literal at
	// turn boundaries.
	pending string

	// turnOpen is true once we've emitted the assistant pill for the
	// current turn. Reset on user message / AssistantDone so the next
	// turn gets its own pill.
	turnOpen bool

	// stripLead, when true, causes the next chunk(s) of streamed text to
	// have their leading whitespace dropped — until the first non-ws
	// byte arrives, at which point the flag clears. Set when a new
	// streaming block opens; absorbs the empty `\n` / `\n\n` Qwen-style
	// templates emit after `</think>` or the assistant turn header.
	stripLead bool

	// followBottom controls auto-scroll behaviour. When true (the
	// default), every appended chunk also scrolls to the bottom so the
	// user sees streaming content land. When false (set by PgUp / Home
	// keystrokes in the host), the view stays put so the user can read
	// scrollback while new content arrives. Re-enabled by End or by
	// submitting a new message.
	followBottom bool

	// inFence is true while we're inside a ```fenced code block``` of
	// assistant content. fenceHold buffers up to 2 trailing backticks
	// at a chunk boundary so a "```" delimiter split across SSE chunks
	// (e.g. chunk 1 ends with "``", chunk 2 starts with "`") still
	// toggles fence state. Both reset on AssistantDone.
	inFence   bool
	fenceHold string

	blocks []chatBlock
	// Pointers to the currently-open streaming blocks, when applicable.
	// Streaming events append to these in place and also paint the view
	// directly; if the user toggles, we rebuild the whole view from blocks.
	curAsst   *assistantBlock
	curReason *reasoningBlock
}

// ensureTurnHeader emits the assistant pill exactly once per turn. Called
// on the first event after a user message that belongs to the assistant
// (reasoning delta, content delta, tool call). Idempotent within a turn.
func (c *ChatDisplay) ensureTurnHeader() {
	if c.turnOpen {
		return
	}
	c.blocks = append(c.blocks, &assistantHeaderBlock{})
	fmt.Fprintf(c.view, "%s\n", pillAsst)
	c.turnOpen = true
}

// NewChatDisplay creates a chat display bound to a TextView.
func NewChatDisplay(view *tview.TextView, modelID string) *ChatDisplay {
	return &ChatDisplay{
		view:         view,
		modelID:      modelID,
		showThinking: true,
		followBottom: true,
	}
}

// SetFollowBottom toggles whether new content auto-scrolls. When set to
// true the view jumps to the end immediately so the user re-engages
// with streaming output; false leaves the scroll position alone.
func (c *ChatDisplay) SetFollowBottom(follow bool) {
	c.followBottom = follow
	if follow {
		c.scrollIfFollowing()
	}
}

// FollowBottom reports the current auto-scroll state.
func (c *ChatDisplay) FollowBottom() bool { return c.followBottom }

// scrollIfFollowing only scrolls to the bottom when the user hasn't
// manually scrolled away. Replaces the previous unconditional
// view.ScrollToEnd() so streaming content doesn't yank the viewport
// away from a user who's reading scrollback.
func (c *ChatDisplay) scrollIfFollowing() {
	if c.followBottom {
		c.view.ScrollToEnd()
	}
}

// ToggleThinking flips the visibility of completed reasoning blocks and
// re-renders the chat from the block model. In-progress reasoning streams
// keep showing live regardless of the flag.
func (c *ChatDisplay) ToggleThinking() {
	c.showThinking = !c.showThinking
	c.redraw()
}

// HasLiveTimerBlock reports whether any tool block is currently running
// past the live-badge threshold. The host's 1s chat ticker uses this to
// gate redraws — a clean idle session takes zero redraws, but a long
// `bash` keeps the elapsed-time counter ticking.
func (c *ChatDisplay) HasLiveTimerBlock() bool {
	for _, b := range c.blocks {
		tb, ok := b.(*toolBlock)
		if !ok || !tb.running() {
			continue
		}
		if time.Since(tb.startedAt) >= liveBadgeThreshold {
			return true
		}
	}
	return false
}

// Redraw is the exported entry point the host's chat ticker uses to
// repaint the elapsed-time badge on in-flight tool calls. Internal
// redraw() stays unexported so other packages don't accidentally
// depend on it for unrelated reasons.
func (c *ChatDisplay) Redraw() { c.redraw() }

// ShowThinking returns the current visibility flag (for status display).
func (c *ChatDisplay) ShowThinking() bool { return c.showThinking }

// redraw clears the view and re-renders every block. Each block is
// wrapped in a tview region tag (`["block-N"]…[""]`) so /find can
// drive ScrollToHighlight without needing a parallel offset map. The
// tags are zero-cost markup (invisible in copy/paste) and the chat's
// TextView already has SetRegions(true) from layout.go.
func (c *ChatDisplay) redraw() {
	c.view.Clear()
	c.stream = streamNone
	for i, b := range c.blocks {
		fmt.Fprintf(c.view, blockRegionOpen, i)
		b.render(c.view, c.showThinking)
		fmt.Fprint(c.view, blockRegionClose)
	}
	c.scrollIfFollowing()
}

// blockRegionOpen / blockRegionClose are tview region markup. The id
// format is `block-<idx>` so HighlightBlock can address blocks by
// their slice position. The closing `[""]` resets the active region
// without affecting any color tags emitted by the block itself.
const (
	blockRegionOpen  = "[\"block-%d\"]"
	blockRegionClose = "[\"\"]"
)

// HighlightBlock scrolls the chat to the block at idx and turns on
// the tview highlight on its region. Callers should ensure a recent
// redraw has happened so the region tags are present in the view —
// the find overlay invokes redraw on open for that reason.
func (c *ChatDisplay) HighlightBlock(idx int) {
	if idx < 0 || idx >= len(c.blocks) {
		return
	}
	c.view.Highlight(fmt.Sprintf("block-%d", idx))
	c.view.ScrollToHighlight()
}

// ClearHighlight turns off any active region highlight. The find
// overlay calls this on Esc / dismiss so the chat returns to its
// unmarked state.
func (c *ChatDisplay) ClearHighlight() {
	c.view.Highlight()
}

// Blocks exposes the current block slice (read-only) so the /find
// overlay can run findInBlocks against it. Returning the slice
// directly is fine since callers only iterate; mutation would be a
// bug regardless of accessor shape.
func (c *ChatDisplay) Blocks() []chatBlock { return c.blocks }

// Render handles a single bus event. Caller is responsible for synchronising
// with the tview application loop (e.g. via Application.QueueUpdateDraw).
func (c *ChatDisplay) Render(evt bus.Event) {
	switch evt.Type {
	case bus.EventUserMessage:
		if msg, ok := evt.Payload.(string); ok {
			c.drainPending()
			c.flushStream()
			c.tagState = streamNone
			c.turnOpen = false
			b := &userBlock{text: msg}
			c.blocks = append(c.blocks, b)
			b.render(c.view, c.showThinking)
			c.scrollIfFollowing()
		}

	case bus.EventReasoningDelta:
		if text, ok := evt.Payload.(string); ok {
			c.ensureTurnHeader()
			c.writeStreamed(text, streamReasoning)
		}

	case bus.EventAssistantDelta:
		if text, ok := evt.Payload.(string); ok {
			c.ensureTurnHeader()
			c.writeStreamed(text, streamAssistant)
		}

	case bus.EventAssistantDone:
		c.drainPending()
		c.flushStream()
		c.tagState = streamNone
		c.turnOpen = false
		c.closeFenceIfOpen()

	case bus.EventToolCallStart:
		if m, ok := evt.Payload.(map[string]any); ok {
			c.ensureTurnHeader()
			c.flushStream()
			id, _ := m["id"].(string)
			name, _ := m["name"].(string)
			b := &toolBlock{id: id, name: name, call: formatToolCall(m), startedAt: time.Now()}
			c.blocks = append(c.blocks, b)
			// Print the call line; the badge is empty here because the
			// call hasn't run long enough to cross the threshold. Output
			// (if any) streams in via EventToolCallProgress and we paint
			// it directly. The 1s chat ticker re-renders this block once
			// the call passes the live-badge threshold. Untrusted-content
			// marker (when the tool's output is external text) goes
			// between the bar and the call name; same `marker` lookup as
			// in (b *toolBlock).render so live-paint and full-redraw
			// agree.
			marker := ""
			if isUntrustedContentTool(b.name) {
				marker = untrustedMarker
			}
			fmt.Fprintf(c.view, "%s%s%s%s%s\n", barTool, marker, b.call, barToolEnd, toolBlockBadge(b))
			c.scrollIfFollowing()
		}

	case bus.EventToolCallProgress:
		if m, ok := evt.Payload.(map[string]any); ok {
			id, _ := m["id"].(string)
			text, _ := m["text"].(string)
			if text == "" {
				return
			}
			// Append to the matching toolBlock so a Ctrl-T redraw still
			// shows the accumulated output.
			for i := len(c.blocks) - 1; i >= 0; i-- {
				if tb, ok := c.blocks[i].(*toolBlock); ok && tb.id == id {
					tb.output += text
					break
				}
			}
			// Live paint: dim, indented, with leader continuation on
			// embedded newlines. Strip trailing \n before painting so we
			// don't emit a stray "  [-]" leader on an empty trailing
			// line, then re-add the newline afterwards.
			tail := strings.TrimRight(text, "\n")
			fmt.Fprintf(c.view, "[gray]  %s[-]", strings.ReplaceAll(tail, "\n", "\n[gray]  "))
			if len(tail) < len(text) {
				fmt.Fprint(c.view, "\n")
			}
			c.scrollIfFollowing()
		}

	case bus.EventToolCallEnd:
		// Close the indented output region with a blank line so the next
		// block (assistant turn, more tool calls) starts on a fresh row.
		// Only emit the spacer if the tool actually produced output —
		// otherwise we already added the trailing \n on the call line.
		// We also stamp the final duration on the toolBlock and force a
		// redraw so the persistent "· 12s" badge replaces any in-flight
		// "· running · 11s" already on screen.
		needRedraw := false
		if m, ok := evt.Payload.(map[string]any); ok {
			id, _ := m["id"].(string)
			for i := len(c.blocks) - 1; i >= 0; i-- {
				if tb, ok := c.blocks[i].(*toolBlock); ok && tb.id == id {
					if !tb.startedAt.IsZero() {
						tb.duration = time.Since(tb.startedAt)
						// Sub-threshold completions don't get a badge,
						// so there's nothing to refresh on screen — skip
						// the redraw cost in the common fast-tool case.
						if tb.duration >= liveBadgeThreshold || tb.duration >= finalBadgeThreshold {
							needRedraw = true
						}
					}
					if tb.output != "" {
						if !strings.HasSuffix(tb.output, "\n") {
							fmt.Fprint(c.view, "\n")
						}
					}
					break
				}
			}
		}
		fmt.Fprint(c.view, "\n")
		if needRedraw {
			c.redraw()
		}
		c.scrollIfFollowing()

	case bus.EventCompacted:
		c.flushStream()
		before, after := compactionTokens(evt.Payload)
		b := &compactedBlock{before: before, after: after}
		c.blocks = append(c.blocks, b)
		b.render(c.view, c.showThinking)
		c.scrollIfFollowing()

	case bus.EventError:
		c.flushStream()
		b := &errorBlock{msg: fmt.Sprint(evt.Payload)}
		c.blocks = append(c.blocks, b)
		b.render(c.view, c.showThinking)
		c.scrollIfFollowing()

	case bus.EventCancelled:
		c.flushStream()
		b := &cancelledBlock{}
		c.blocks = append(c.blocks, b)
		b.render(c.view, c.showThinking)
		c.scrollIfFollowing()

	case bus.EventInputDiscarded:
		// Local-mode payloads carry an int; daemon-attached events
		// arrive as float64 after JSON round-trip. tokenField in this
		// file already handles both; reuse it.
		count := tokenField(evt.Payload)
		if count <= 0 {
			return
		}
		c.flushStream()
		b := &inputDiscardedBlock{count: count}
		c.blocks = append(c.blocks, b)
		b.render(c.view, c.showThinking)
		c.scrollIfFollowing()
	}
}

// drainPending emits any held tag-prefix bytes as literal text in the
// current stream mode (or as an assistant block if no stream is open). Used
// at turn boundaries when we know the in-progress chunk won't be completed
// — better to render the bytes than silently drop them.
func (c *ChatDisplay) drainPending() {
	if c.pending == "" {
		return
	}
	mode := c.stream
	if mode == streamNone {
		mode = streamAssistant
	}
	c.emitChunk(c.pending, mode)
	c.pending = ""
}

// flushStream closes the currently-open streaming block. Live streaming
// painted the bar/text directly; here we finalise the parallel block (stamp
// duration on reasoning) and write the trailing separator.
func (c *ChatDisplay) flushStream() {
	switch c.stream {
	case streamAssistant:
		fmt.Fprint(c.view, "\n\n")
	case streamReasoning:
		if c.curReason != nil {
			c.curReason.duration = time.Since(c.curReason.started)
			c.curReason.closed = true
			fmt.Fprintf(c.view, "%s\n[comment]▎ thought for %s[-]\n\n",
				barReasonEnd, fmtDuration(c.curReason.duration))
		} else {
			fmt.Fprint(c.view, barReasonEnd, "\n\n")
		}
	}
	c.stream = streamNone
	c.curAsst = nil
	c.curReason = nil
}

// writeStreamed appends a chunk of streaming text from the given source
// channel. Mode is decided by, in order:
//
//  1. tagState (set by a previous `<think>` or `</think>` marker), if non-zero
//  2. the delta's source channel (reasoning_content vs content)
//
// Tags are scanned for and stripped from output. Either tag, in either
// channel, drives a mode flip — that's important because llama.cpp's split
// between reasoning_content and content is unreliable across templates.
//
// Trailing bytes that could be the start of a tag (e.g. `</thi`) are held
// in c.pending and prepended on the next delta, so a tag split across SSE
// chunks is still recognised.
func (c *ChatDisplay) writeStreamed(text string, source streamMode) {
	if c.pending != "" {
		text = c.pending + text
		c.pending = ""
	}

	for len(text) > 0 {
		mode := source
		if c.tagState != streamNone {
			mode = c.tagState
		}

		idx, tagLen, nextState := nextThinkTag(text)
		if idx < 0 {
			// Hold a trailing partial-tag suffix for the next delta.
			if hold := partialTagSuffix(text); hold > 0 {
				c.pending = text[len(text)-hold:]
				text = text[:len(text)-hold]
			}
			c.emitChunk(text, mode)
			return
		}
		c.emitChunk(text[:idx], mode)
		text = text[idx+tagLen:]
		c.tagState = nextState
	}
}

// partialTagSuffix returns the byte length of the longest suffix of `s`
// that is a strict prefix of `<think>` or `</think>`. Returns 0 if no
// suffix could be the start of a tag.
func partialTagSuffix(s string) int {
	longest := 0
	for _, tag := range []string{thinkOpen, thinkClose} {
		max := len(tag) - 1
		if max > len(s) {
			max = len(s)
		}
		for i := max; i > 0; i-- {
			if strings.HasPrefix(tag, s[len(s)-i:]) {
				if i > longest {
					longest = i
				}
				break
			}
		}
	}
	return longest
}

// nextThinkTag finds the first `<think>` or `</think>` tag in text and
// returns its byte offset, length, and the new state to enter.
func nextThinkTag(text string) (idx, tagLen int, state streamMode) {
	openIdx := strings.Index(text, thinkOpen)
	closeIdx := strings.Index(text, thinkClose)
	switch {
	case openIdx < 0 && closeIdx < 0:
		return -1, 0, streamNone
	case openIdx < 0:
		return closeIdx, len(thinkClose), streamAssistant
	case closeIdx < 0:
		return openIdx, len(thinkOpen), streamReasoning
	case closeIdx < openIdx:
		return closeIdx, len(thinkClose), streamAssistant
	default:
		return openIdx, len(thinkOpen), streamReasoning
	}
}

// emitChunk writes `text` in the given streaming mode, opening a new block
// (with leader) on first byte, and ensuring the leader is repeated on
// every embedded newline so the bar runs uninterrupted. Also appends the
// raw text to the parallel block model.
//
// Leading whitespace at the very start of a freshly-opened block is
// trimmed: Qwen's chat template (and others) tend to prefix content
// with one or two newlines after `</think>` or after the assistant's
// turn header, which otherwise show up as a gap below the pill.
func (c *ChatDisplay) emitChunk(text string, mode streamMode) {
	if text == "" {
		return
	}
	if c.stream != mode {
		c.flushStream()
		switch mode {
		case streamReasoning:
			b := &reasoningBlock{started: time.Now()}
			c.blocks = append(c.blocks, b)
			c.curReason = b
			fmt.Fprint(c.view, barReasoning)
		case streamAssistant:
			b := &assistantBlock{}
			c.blocks = append(c.blocks, b)
			c.curAsst = b
			// Pill is emitted once per turn by ensureTurnHeader; nothing
			// to print here when an assistant content stream opens.
		}
		c.stream = mode
		c.stripLead = true
	}
	if c.stripLead {
		text = strings.TrimLeft(text, " \t\r\n")
		if text == "" {
			return
		}
		c.stripLead = false
	}

	switch mode {
	case streamReasoning:
		if c.curReason != nil {
			c.curReason.text += text
		}
		text = strings.ReplaceAll(text, "\n", "\n"+barReasoning)
	case streamAssistant:
		// Convert raw model output into render-safe form: ```fenced```
		// content gets a [comment] tag, brackets inside fences are
		// escaped so code like []int doesn't get eaten by tview's
		// color-tag parser. The processed string is what we both
		// store (so Ctrl-T redraw works) and write to the view.
		text = c.processAssistantChunk(text)
		if text == "" {
			return
		}
		if c.curAsst != nil {
			c.curAsst.text += text
		}
	}

	fmt.Fprint(c.view, text)
	c.scrollIfFollowing()
}

// processAssistantChunk applies fenced-code styling to a streamed
// assistant chunk. State (c.inFence, c.fenceHold) carries across calls
// so a "```" delimiter split across SSE chunks still toggles correctly.
//
// Returned text is render-safe: fence boundaries are wrapped in
// [comment]…[-] color tags (the delimiter itself is consumed) and
// brackets inside fences are escaped to "[[", which tview renders as
// a literal `[`. Outside fences, content passes through unchanged so
// the existing "model emits literal [tag]" behaviour is preserved
// (worth fixing eventually, but not what this pass is about).
func (c *ChatDisplay) processAssistantChunk(text string) string {
	text = c.fenceHold + text
	c.fenceHold = ""

	// Defer trailing backticks: they could be the start of a "```"
	// delimiter completed by the next chunk. Up to 2 chars held back.
	if hold := trailingBackticks(text, 2); hold > 0 {
		c.fenceHold = text[len(text)-hold:]
		text = text[:len(text)-hold]
	}

	if text == "" {
		return ""
	}

	var sb strings.Builder
	for {
		idx := strings.Index(text, "```")
		if idx < 0 {
			if c.inFence {
				sb.WriteString(escapeBrackets(text))
			} else {
				sb.WriteString(text)
			}
			return sb.String()
		}
		before := text[:idx]
		if c.inFence {
			sb.WriteString(escapeBrackets(before))
			sb.WriteString("[-]") // close fence color
		} else {
			sb.WriteString(before)
		}
		c.inFence = !c.inFence
		if c.inFence {
			sb.WriteString("[comment]") // open fence color
		}
		text = text[idx+3:] // skip the delimiter itself
	}
}

// closeFenceIfOpen flushes any held fence-delimiter buffer and emits a
// closing color tag if the assistant ended a turn mid-fence. Without
// this an unterminated ```fence in the model output would bleed the
// fence color into subsequent content (tool calls, the next turn).
func (c *ChatDisplay) closeFenceIfOpen() {
	if c.fenceHold != "" {
		// The held bytes (up to 2 backticks) never completed a "```",
		// so emit them literally — escape inside a fence so they
		// render as backticks rather than tag bait.
		held := c.fenceHold
		c.fenceHold = ""
		if c.inFence {
			held = escapeBrackets(held)
		}
		fmt.Fprint(c.view, held)
		if c.curAsst != nil {
			c.curAsst.text += held
		}
	}
	if c.inFence {
		fmt.Fprint(c.view, "[-]")
		if c.curAsst != nil {
			c.curAsst.text += "[-]"
		}
		c.inFence = false
	}
}

// renderSafeAssistant is the stateless equivalent of
// processAssistantChunk for whole-string conversion. Used by
// ReplayHistory when re-rendering a complete persisted assistant
// message. Closes any unterminated fence at end of input.
func renderSafeAssistant(s string) string {
	var sb strings.Builder
	inFence := false
	for {
		idx := strings.Index(s, "```")
		if idx < 0 {
			if inFence {
				sb.WriteString(escapeBrackets(s))
				sb.WriteString("[-]")
			} else {
				sb.WriteString(s)
			}
			return sb.String()
		}
		before := s[:idx]
		if inFence {
			sb.WriteString(escapeBrackets(before))
			sb.WriteString("[-]")
		} else {
			sb.WriteString(before)
		}
		inFence = !inFence
		if inFence {
			sb.WriteString("[comment]")
		}
		s = s[idx+3:]
	}
}

// trailingBackticks returns the count of trailing `\“ characters in s,
// up to max. Used to decide how many bytes to defer to the next chunk
// when a chunk ends with what could be a partial fence delimiter.
func trailingBackticks(s string, max int) int {
	n := 0
	for i := len(s) - 1; i >= 0 && n < max && s[i] == '`'; i-- {
		n++
	}
	return n
}

// escapeBrackets escapes "[" → "[[" so tview renders bracketed text
// literally instead of trying to parse it as a color tag. Bare "]"
// doesn't need escaping (tview's parser uses "[" as the only
// delimiter). Used inside fenced code blocks where things like
// `[]int` are common.
func escapeBrackets(s string) string {
	return strings.ReplaceAll(s, "[", "[[")
}

// prefixLines applies a bar prefix to every line of `text`, including the
// first. Used for full-content blocks (user messages, tool calls).
func prefixLines(prefix, text string) string {
	if text == "" {
		return prefix + "\n"
	}
	lines := strings.Split(text, "\n")
	var b strings.Builder
	for i, ln := range lines {
		b.WriteString(prefix)
		b.WriteString(ln)
		if i < len(lines)-1 {
			b.WriteByte('\n')
		}
	}
	return b.String() + "\n"
}

// formatToolCall renders an EventToolCallStart payload as `name(arg=val, ...)`.
func formatToolCall(m map[string]any) string {
	name, _ := m["name"].(string)
	args, _ := m["args"].(map[string]any)
	if len(args) == 0 {
		return name + "()"
	}
	var parts []string
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 60 {
			s = s[:60] + "…"
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

// ReplayHistory renders an existing message list (e.g. from a resumed
// session) into the chat view as a one-shot snapshot. Reasoning is not
// persisted, so resumed sessions never show thought blocks.
func (c *ChatDisplay) ReplayHistory(history []llm.Message, modelID string) {
	for _, m := range history {
		switch m.Role {
		case "system":
			continue
		case "user":
			b := &userBlock{text: m.Content}
			c.blocks = append(c.blocks, b)
			b.render(c.view, c.showThinking)
		case "assistant":
			// Replay needs the same once-per-turn pill the live path
			// emits — without it, resumed sessions show body text and
			// tool calls floating without an ENSŌ label.
			if m.Content != "" || len(m.ToolCalls) > 0 {
				h := &assistantHeaderBlock{}
				c.blocks = append(c.blocks, h)
				h.render(c.view, c.showThinking)
			}
			if m.Content != "" {
				// Replay through the fence-styling filter so resumed
				// sessions get the same rendering as live streamed
				// turns — bracketed code stays intact and ```fenced```
				// regions pick up the [comment] color tag.
				b := &assistantBlock{text: renderSafeAssistant(m.Content)}
				c.blocks = append(c.blocks, b)
				b.render(c.view, c.showThinking)
			}
			for _, tc := range m.ToolCalls {
				b := &toolBlock{name: tc.Function.Name, call: tc.Function.Name + "()"}
				c.blocks = append(c.blocks, b)
				b.render(c.view, c.showThinking)
			}
		case "tool":
			snippet := m.Content
			if len(snippet) > 200 {
				snippet = snippet[:200] + "…"
			}
			fmt.Fprintf(c.view, "[teal]  %s[-]\n\n", snippet)
		}
	}
	c.scrollIfFollowing()
}

// toolBlockBadge returns the trailing " · running · 12s" / " · 12s"
// segment for a tool block, or "" when nothing should render. Pulled
// out of (b *toolBlock).render so the live-paint path at
// EventToolCallStart can also call it (the call line is written once
// directly to the view, then a 1s ticker triggers redraw to update
// the badge — everything that paints the same line uses this helper
// to stay consistent).
func toolBlockBadge(b *toolBlock) string {
	switch {
	case b.running():
		e := b.elapsed()
		if e < liveBadgeThreshold {
			return ""
		}
		return fmt.Sprintf(" [gray]· running · %s[-]", fmtToolElapsed(e))
	case b.duration >= finalBadgeThreshold:
		return fmt.Sprintf(" [gray]· %s[-]", fmtToolElapsed(b.duration))
	}
	return ""
}

// fmtToolElapsed renders an elapsed duration at second precision —
// "12s" / "2m05s". Used for tool-call badges where sub-second jitter
// would just be noise; reasoning blocks still use fmtDuration's
// fractional form because their durations are usually short.
func fmtToolElapsed(d time.Duration) string {
	secs := int(d.Seconds())
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return fmt.Sprintf("%dm%02ds", secs/60, secs%60)
}

// fmtDuration prints a short, scannable duration like "1.2s" or "850ms".
func fmtDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
