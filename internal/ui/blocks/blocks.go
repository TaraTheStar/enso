// SPDX-License-Identifier: AGPL-3.0-or-later

// Package blocks holds the framework-agnostic data types for chat
// blocks shared by both UI backends. Each backend implements rendering
// separately — tview emits color-tag markup into a TextView, bubble
// emits Lipgloss-styled strings via tea.Println for scrollback
// graduation. The streaming state machine that drives mutations lives
// in each backend's internal package; only the shared block shapes and
// pure helpers live here.
//
// Mutation contract: blocks are mutable while live (the most-recent
// block in a turn accumulates streaming deltas) and frozen once
// graduated to scrollback. Backends are responsible for not retaining
// references after graduation.
package blocks

import (
	"time"

	"github.com/TaraTheStar/enso/internal/llm"
)

// Block is the marker interface for chat block types. The marker is a
// private method (isBlock) so external packages can't accidentally
// satisfy it — Block is closed over the types in this package.
type Block interface {
	isBlock()
}

// User is the user's message at the top of a turn.
type User struct {
	Text string
}

// Assistant is the assistant's body content for a turn. A single turn
// can produce multiple Assistant blocks if interleaved with tool calls
// or reasoning.
type Assistant struct {
	Text string
}

// Tool represents one tool call within a turn. Output accumulates from
// progress events; Duration is set on completion.
type Tool struct {
	ID        string
	Name      string        // tool name (e.g. "Bash", "Read", "lsp_hover")
	Call      string        // formatted "name(arg=val, ...)" for display
	Output    string        // accumulated stdout/stderr from progress events
	StartedAt time.Time     // wall-clock time the call began (zero on replay)
	Duration  time.Duration // zero while running; final duration once End fires
}

// Running reports whether this block represents an in-flight tool call.
// Replay paths leave StartedAt zero, so they never look "running" even
// though Duration is also zero.
func (b *Tool) Running() bool {
	return !b.StartedAt.IsZero() && b.Duration == 0
}

// Elapsed is the duration to display: live time-since for running
// calls, the recorded final duration for completed ones. Zero for
// replayed/legacy blocks where StartedAt was never set.
func (b *Tool) Elapsed() time.Duration {
	if b.StartedAt.IsZero() {
		return 0
	}
	if b.Duration > 0 {
		return b.Duration
	}
	return time.Since(b.StartedAt)
}

// Reasoning is the assistant's chain-of-thought, surfaced as a
// dim/recede block above the main response.
type Reasoning struct {
	Text     string
	Started  time.Time
	Duration time.Duration // zero while still open
	Closed   bool
}

// Error is a turn-level failure. APIErr is non-nil for HTTP
// 4xx/5xx responses from the model provider; RetryAt is the
// wall-clock deadline for any Retry-After countdown.
type Error struct {
	Msg     string
	APIErr  *llm.APIError
	RetryAt time.Time
}

// Cancelled marks a user-initiated turn cancellation.
type Cancelled struct{}

// InputDiscarded marks queued user messages that piled up during a
// cancelled turn and were drained rather than processed.
type InputDiscarded struct {
	Count int
}

// Compacted marks a context-compaction boundary with the token counts
// before/after.
type Compacted struct {
	Before int
	After  int
}

func (*User) isBlock()           {}
func (*Assistant) isBlock()      {}
func (*Tool) isBlock()           {}
func (*Reasoning) isBlock()      {}
func (*Error) isBlock()          {}
func (*Cancelled) isBlock()      {}
func (*InputDiscarded) isBlock() {}
func (*Compacted) isBlock()      {}
