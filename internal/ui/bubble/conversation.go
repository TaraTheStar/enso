// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"fmt"
	"strings"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/ui/blocks"
)

// conversation is the bubble-side state machine that owns the most
// recent in-flight block. As bus events arrive, HandleEvent mutates the
// live block, opens a new one, and/or returns a slice of blocks that
// should be graduated to scrollback (rendered and tea.Println'd by the
// caller).
//
// At most one block is "live" at a time. When a new block starts (e.g.,
// EventToolCallStart while an assistant block is still streaming), the
// previous live block graduates first so scrollback order matches the
// agent's actual output sequence.
type conversation struct {
	history []blocks.Block // past graduated blocks (for /find, replay, etc.)
	live    blocks.Block   // currently-streaming block; nil if none
}

// Live returns the in-flight block (if any) for the live region to
// render. The caller treats this read-only.
func (c *conversation) Live() blocks.Block { return c.live }

// History returns the past graduated blocks of the current session in
// order. /find searches over this; bubble has no other handle on its
// own scrollback.
func (c *conversation) History() []blocks.Block { return c.history }

// Append records a block in history without changing the live state.
// Used for blocks that don't flow through the bus event machine —
// notably the user's typed messages (echoed via tea.Println directly).
func (c *conversation) Append(b blocks.Block) {
	if b == nil {
		return
	}
	c.history = append(c.history, b)
}

// HandleEvent advances the conversation state for one bus event and
// returns any blocks that should be graduated to scrollback, in order.
// Graduations happen for two reasons:
//  1. The current event ends the live block (e.g., AssistantDone).
//  2. The current event opens a different kind of block, so the
//     previous live block graduates before the new one becomes live.
func (c *conversation) HandleEvent(ev bus.Event) []blocks.Block {
	switch ev.Type {
	case bus.EventAssistantDelta:
		s, _ := ev.Payload.(string)
		if s == "" {
			return nil
		}
		if a, ok := c.live.(*blocks.Assistant); ok {
			a.Text += s
			return nil
		}
		out := c.flushLive()
		c.live = &blocks.Assistant{Text: s}
		return out

	case bus.EventReasoningDelta:
		s, _ := ev.Payload.(string)
		if s == "" {
			return nil
		}
		if r, ok := c.live.(*blocks.Reasoning); ok {
			r.Text += s
			return nil
		}
		out := c.flushLive()
		c.live = &blocks.Reasoning{Text: s, Started: time.Now()}
		return out

	case bus.EventAssistantDone:
		// The agent finished one LLM completion. If it was a content
		// turn we graduate the assistant block; if it was a tool-call
		// turn the assistant block is empty and skipped. Reasoning
		// blocks survive until the next non-reasoning event.
		if _, ok := c.live.(*blocks.Reasoning); ok {
			return nil
		}
		return c.flushLive()

	case bus.EventToolCallStart:
		d, ok := ev.Payload.(map[string]any)
		if !ok {
			return nil
		}
		out := c.flushLive()
		c.live = &blocks.Tool{
			ID:        getString(d, "id"),
			Name:      getString(d, "name"),
			Call:      formatToolCall(d),
			StartedAt: time.Now(),
		}
		return out

	case bus.EventToolCallProgress:
		d, ok := ev.Payload.(map[string]any)
		if !ok {
			return nil
		}
		t, ok := c.live.(*blocks.Tool)
		if !ok || t.ID != getString(d, "id") {
			return nil
		}
		t.Output += getString(d, "chunk")
		return nil

	case bus.EventToolCallEnd:
		d, ok := ev.Payload.(map[string]any)
		if !ok {
			return nil
		}
		t, ok := c.live.(*blocks.Tool)
		if !ok {
			return nil
		}
		if id := getString(d, "id"); id != "" && id != t.ID {
			return nil
		}
		if denied, _ := d["denied"].(bool); denied {
			t.Output = "(denied)"
		} else if result := getString(d, "result"); result != "" {
			t.Output = result
		}
		t.Duration = time.Since(t.StartedAt)
		return c.flushLive()

	case bus.EventError:
		out := c.flushLive()
		eb := &blocks.Error{Msg: payloadAsString(ev.Payload)}
		if apiErr, ok := ev.Payload.(*llm.APIError); ok {
			eb.APIErr = apiErr
			eb.Msg = apiErr.Error()
		}
		c.history = append(c.history, eb)
		out = append(out, eb)
		return out

	case bus.EventCancelled:
		out := c.flushLive()
		cb := &blocks.Cancelled{}
		c.history = append(c.history, cb)
		out = append(out, cb)
		return out

	case bus.EventCompacted:
		out := c.flushLive()
		before, after := compactionTokens(ev.Payload)
		cmp := &blocks.Compacted{Before: before, After: after}
		c.history = append(c.history, cmp)
		out = append(out, cmp)
		return out

	case bus.EventInputDiscarded:
		n, ok := ev.Payload.(int)
		if !ok || n <= 0 {
			return nil
		}
		idb := &blocks.InputDiscarded{Count: n}
		c.history = append(c.history, idb)
		return []blocks.Block{idb}

	case bus.EventAgentIdle:
		// Pipeline finished. Flush any straggler (defensive — usually
		// nothing live by this point).
		return c.flushLive()

	case bus.EventAgentStart, bus.EventAgentEnd:
		// Subagent lifecycle — surfaced as inline notices in
		// model.handleBusEvent rather than through the conversation
		// state, since they're cross-turn annotations rather than
		// chat content. The conversation does NOT track subagent
		// streaming (their tokens fan out on a separate transcript).
		return nil
	}
	return nil
}

// flushLive returns and clears the live block, also appending it to
// history so /find and replay can see it later. Returns the same
// block(s) for the caller to render and tea.Println.
func (c *conversation) flushLive() []blocks.Block {
	if c.live == nil {
		return nil
	}
	// Mark reasoning blocks closed before they graduate so the
	// renderer can switch to the "thought for N.Ns" footer form.
	if r, ok := c.live.(*blocks.Reasoning); ok && !r.Closed {
		r.Closed = true
		if !r.Started.IsZero() {
			r.Duration = time.Since(r.Started)
		}
	}
	out := []blocks.Block{c.live}
	c.history = append(c.history, c.live)
	c.live = nil
	return out
}

// formatToolCall renders the call signature for display: "name(arg=val, ...)".
// args missing or empty produces just "name()".
func formatToolCall(d map[string]any) string {
	name := getString(d, "name")
	if name == "" {
		name = "tool"
	}
	args, ok := d["args"].(map[string]any)
	if !ok || len(args) == 0 {
		return name + "()"
	}
	// Stable order keeps repeat calls consistent. Map iteration in Go
	// is randomised, so we sort.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	// Tiny inline sort — avoid pulling sort just for this.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, summarizeArg(args[k])))
	}
	return name + "(" + strings.Join(parts, ", ") + ")"
}

// summarizeArg flattens a JSON-y argument into a short display form.
// Long strings get truncated; non-strings render via fmt.Sprintf("%v").
func summarizeArg(v any) string {
	const max = 40
	s := fmt.Sprintf("%v", v)
	if len(s) > max {
		return s[:max-1] + "…"
	}
	return s
}

// getString reads a string field from a payload map; returns "" when
// missing or wrong type.
func getString(d map[string]any, key string) string {
	if v, ok := d[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// payloadAsString coerces an arbitrary error payload into a display
// string. Both publishers send errors as either error values or strings.
func payloadAsString(p any) string {
	switch v := p.(type) {
	case nil:
		return ""
	case error:
		return v.Error()
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

// compactionTokens parses an EventCompacted payload's before/after
// token counts. Daemon JSON deserialises ints as float64, so all three
// numeric forms are accepted.
func compactionTokens(payload any) (before, after int) {
	d, ok := payload.(map[string]any)
	if !ok {
		return 0, 0
	}
	return tokenField(d["before"]), tokenField(d["after"])
}

func tokenField(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case float64:
		return int(x)
	}
	return 0
}
