// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"fmt"
	"sync"

	"github.com/TaraTheStar/enso/internal/bus"
)

// ActivityState describes what the agent (or attach client) is doing
// right now. Drives the left-side status indicator: a glyph (animated
// when busy) plus a short label, both colored to match the chat palette.
type ActivityState int

const (
	ActivityReady        ActivityState = iota // idle; static green dot
	ActivitySubmitting                        // user submitted, awaiting first event
	ActivityThinking                          // reasoning stream is open
	ActivityGenerating                        // assistant content stream is open
	ActivityToolCall                          // a tool is currently executing
	ActivityCancelled                         // last turn was cancelled
	ActivityError                             // last turn errored
	ActivityConnecting                        // attach mode: dialing daemon
	ActivityReconnecting                      // attach mode: re-dialing daemon
)

// spinnerFrames are the unicode braille dots stepped at ~100ms — reads
// as a smooth shimmer at terminal frame rates without nerd-font glyphs.
var spinnerFrames = []rune{'⠋', '⠙', '⠹', '⠸', '⠼', '⠴', '⠦', '⠧', '⠇', '⠏'}

// Activity is the live agent-state shown in the status bar. Concurrent-
// safe: the bus subscriber, the spinner ticker, and the input handler
// all touch it.
type Activity struct {
	mu    sync.Mutex
	state ActivityState
	tool  string
	frame int
}

// NewActivity constructs a fresh Activity in the ready state.
func NewActivity() *Activity {
	return &Activity{state: ActivityReady}
}

// Set transitions to the given state, returning true iff the visible
// representation changed (so callers can skip status redraws on every
// streaming delta when the state name is the same). `tool` is consulted
// only for ActivityToolCall and ignored otherwise.
func (a *Activity) Set(state ActivityState, tool string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	newTool := ""
	if state == ActivityToolCall {
		newTool = tool
	}
	if a.state == state && a.tool == newTool {
		return false
	}
	a.state = state
	a.tool = newTool
	if !isBusyState(state) {
		a.frame = 0
	}
	return true
}

// State returns the current state (lock-free read would race the
// setter, so we go through the mutex).
func (a *Activity) State() ActivityState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// IsBusy reports whether the indicator is in an animated state. Used by
// the spinner ticker to gate its work.
func (a *Activity) IsBusy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return isBusyState(a.state)
}

// Tick advances the spinner frame. No-op when not busy. Caller is
// responsible for triggering a status redraw afterward.
func (a *Activity) Tick() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !isBusyState(a.state) {
		return
	}
	a.frame = (a.frame + 1) % len(spinnerFrames)
}

// Render returns a tview-tagged string for the activity segment of the
// status bar. No leading or trailing whitespace — caller composes.
func (a *Activity) Render() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	color, glyph, label := activityVisuals(a.state, a.tool, a.frame)
	return fmt.Sprintf("[%s]%s[-] %s", color, glyph, label)
}

// activityVisuals decides the color/glyph/label triple for a given
// state. Pulled out so it's exercised cleanly in tests.
func activityVisuals(state ActivityState, tool string, frame int) (color, glyph, label string) {
	spin := func() string {
		idx := frame
		if idx < 0 || idx >= len(spinnerFrames) {
			idx = 0
		}
		return string(spinnerFrames[idx])
	}
	switch state {
	case ActivityReady:
		return "sage", "●", "ready"
	case ActivitySubmitting:
		return "lavender", spin(), "thinking…"
	case ActivityThinking:
		return "lavender", spin(), "thinking"
	case ActivityGenerating:
		return "lavender", spin(), "generating"
	case ActivityToolCall:
		if tool != "" {
			return "teal", spin(), "running " + tool
		}
		return "teal", spin(), "running…"
	case ActivityCancelled:
		return "dust", "●", "cancelled"
	case ActivityError:
		return "red", "●", "error"
	case ActivityConnecting:
		return "dust", spin(), "connecting…"
	case ActivityReconnecting:
		return "dust", spin(), "reconnecting…"
	}
	return "gray", "●", ""
}

// activityLabel returns the plain label for a state — used by the
// status-line template's `.Activity` field, which is plain text, not a
// tview-tagged string.
func activityLabel(state ActivityState) string {
	_, _, label := activityVisuals(state, "", 0)
	return label
}

// updateActivityFromEvent maps a bus event onto an Activity state
// transition. Returns true iff the visible representation actually
// changed (so callers can skip status redraws on streaming deltas that
// don't change the indicator). Does NOT touch connect / reconnect
// transitions — those come from the attach client's lifecycle, not the
// event stream.
func updateActivityFromEvent(a *Activity, ev bus.Event) bool {
	switch ev.Type {
	case bus.EventReasoningDelta:
		return a.Set(ActivityThinking, "")
	case bus.EventAssistantDelta:
		return a.Set(ActivityGenerating, "")
	case bus.EventToolCallStart:
		name := ""
		if m, ok := ev.Payload.(map[string]any); ok {
			name, _ = m["name"].(string)
		}
		return a.Set(ActivityToolCall, name)
	case bus.EventToolCallEnd:
		// Tool finished; the agent is back to "working" until the next
		// delta or tool start clarifies what kind of work.
		return a.Set(ActivitySubmitting, "")
	case bus.EventAgentIdle:
		// Don't overwrite a terminal Error/Cancelled state — those events
		// fire from the same pipeline exit and should win the indicator.
		if a.State() == ActivityError || a.State() == ActivityCancelled {
			return false
		}
		return a.Set(ActivityReady, "")
	case bus.EventCancelled:
		return a.Set(ActivityCancelled, "")
	case bus.EventError:
		return a.Set(ActivityError, "")
	}
	return false
}

func isBusyState(s ActivityState) bool {
	switch s {
	case ActivitySubmitting, ActivityThinking, ActivityGenerating,
		ActivityToolCall, ActivityConnecting, ActivityReconnecting:
		return true
	}
	return false
}
