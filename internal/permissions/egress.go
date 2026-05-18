// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

// EgressDecision is the user's answer to an interactive egress prompt.
// It mirrors the tool-permission y/n/turn shape, scoped to network
// egress: the only "remember" granularity that makes sense for a
// per-task sealed box is "for the rest of this task" (there is no
// durable egress allowlist file to persist into — that is the static
// [backend.egress] allow list, edited up front, not here).
type EgressDecision int

const (
	EgressDeny      EgressDecision = iota // 403 the connection
	EgressAllowOnce                       // allow this target now
	EgressAllowTask                       // allow this target for the rest of the task
)

// EgressPrompt is the bus payload for bus.EventEgressRequest. The
// host-side InteractiveBroker publishes it when a sealed box tries to
// reach a target that is not on the static allowlist; a subscriber
// (the TUI) asks the user and sends the answer on Respond. The broker
// blocks on Respond until a value arrives (or the originating request's
// context is cancelled), exactly like PromptRequest.
//
// Respond MUST be buffered (cap 1) by the publisher so a slow or gone
// subscriber can never wedge the broker goroutine.
type EgressPrompt struct {
	Target  string // "host:port" the box is trying to reach
	Reason  string // human-facing why (originating tool / command), best-effort
	Respond chan EgressDecision
}
