// SPDX-License-Identifier: AGPL-3.0-or-later

// Package wire holds the llm-typed payloads carried over the Backend
// Channel for host-proxied inference. It is the single source of truth
// for inference-stream framing, imported by BOTH the worker-side
// adapter (internal/backend/worker) and the host-side adapter
// (internal/backend/host) so the two cannot drift on the shape.
//
// It sits above internal/backend (which stays stdlib-only at the bottom
// of the import graph) and below the host/worker adapters. It may
// import llm; internal/backend may not.
package wire

import (
	"encoding/json"

	"github.com/TaraTheStar/enso/internal/llm"
)

// InferenceRequest is the body of a MsgInferenceRequest. Provider names
// which configured endpoint the host must dial: the worker carries no
// endpoint/key and never picks a real provider, so the choice has to
// travel explicitly (a bare llm.ChatRequest only carries Model, which
// is not a unique provider key).
type InferenceRequest struct {
	Provider string          `json:"provider"`
	Request  llm.ChatRequest `json:"request"`
}

// LLMEvent is the serializable form of llm.Event, whose Error is an
// `error` (not JSON-safe) and whose Type is an unexported-friendly int.
// It is the body of a MsgInferenceEvent.
type LLMEvent struct {
	Type      int            `json:"type"`
	Text      string         `json:"text,omitempty"`
	ToolCalls []llm.ToolCall `json:"tool_calls,omitempty"`
	Error     string         `json:"error,omitempty"`
}

// FromLLM converts an llm.Event into its wire form.
func FromLLM(ev llm.Event) LLMEvent {
	w := LLMEvent{
		Type:      int(ev.Type),
		Text:      ev.Text,
		ToolCalls: ev.ToolCalls,
	}
	if ev.Error != nil {
		w.Error = ev.Error.Error()
	}
	return w
}

// ToLLM converts a wire event back into an llm.Event. The error string
// is rehydrated as a plain error (the original error type does not
// survive a process boundary, by design).
func (w LLMEvent) ToLLM() llm.Event {
	ev := llm.Event{
		Type:      llm.EventType(w.Type),
		Text:      w.Text,
		ToolCalls: w.ToolCalls,
	}
	if w.Error != "" {
		ev.Error = errString(w.Error)
	}
	return ev
}

// errString is a minimal error carrying a message across the seam.
type errString string

func (e errString) Error() string { return string(e) }

// PermissionRequest is the body of a MsgPermissionRequest: the
// serializable subset of permissions.PromptRequest (whose Respond is a
// live channel). Shared by both adapters so they cannot drift.
type PermissionRequest struct {
	Tool      string         `json:"tool"`
	ArgString string         `json:"arg_string,omitempty"`
	Args      map[string]any `json:"args,omitempty"`
	Diff      string         `json:"diff,omitempty"`
	AgentID   string         `json:"agent_id,omitempty"`
	AgentRole string         `json:"agent_role,omitempty"`
}

// PermissionDecision is the body of a MsgPermissionDecision. Decision
// must be exactly PermAllow; anything else is treated as deny
// (fail-closed), matching the daemon protocol's discipline.
type PermissionDecision struct {
	Decision string `json:"decision"`
}

const (
	PermAllow = "allow"
	PermDeny  = "deny"
)

// Telemetry is the body of a MsgTelemetry: the worker→host snapshot of
// the agent state the TUI status line / overlay used to read directly
// off *agent.Agent. It deliberately carries ONLY what the worker
// uniquely knows — token accounting and which provider the agent
// selected. The active provider's configured context window and its
// live transport conn-state are filled in host-side from the REAL
// provider (the worker is credential-scrubbed: it holds no configured
// window and no live client). Emitted on change, coalesced.
//
// Comparable by design (all fields are comparable) so the worker can
// suppress no-op re-sends with a plain ==.
type Telemetry struct {
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	EstTokens int    `json:"est_tokens"`
	CumIn     int64  `json:"cum_in"`
	CumOut    int64  `json:"cum_out"`
}

// ---- control RPC (MsgControlRequest / MsgControlResponse) ----------
//
// The TUI's slash commands / overlay drive the worker-side agent
// synchronously: switch provider, compact, prune, inspect the prefix
// breakdown, set next-turn tools. These mutate or read the agent's
// live history/state, which lives only in the worker. They are carried
// over ONE generic correlated request/response pair with a method
// namespace inside, so adding a verb never re-versions the taxonomy.
//
// Read-only token/provider numbers do NOT go here — those ride the
// coalesced Telemetry leg. Provider context (the "## Available models"
// derivation) is rebuilt host-side from the real providers, so it
// needs no RPC either.

// Control method names. Stable strings (the wire namespace).
const (
	CtrlSetProvider      = "set_provider"        // Args: ControlName;        Result: none (Error on unknown)
	CtrlCompactPreview   = "compact_preview"     // Args: none;               Result: CompactPreview
	CtrlForceCompact     = "force_compact"       // Args: none;               Result: ForceCompactResult
	CtrlForcePrune       = "force_prune"         // Args: none;               Result: ForcePruneResult
	CtrlPrefixBreakdown  = "prefix_breakdown"    // Args: none;               Result: PrefixBreakdown
	CtrlSetNextTurnTools = "set_next_turn_tools" // Args: ControlNames;      Result: none
	CtrlSetYolo          = "set_yolo"            // Args: ControlBool;       Result: none
)

// ControlBool is the args payload for single-bool methods.
type ControlBool struct {
	Value bool `json:"value"`
}

// ControlRequest is the body of a MsgControlRequest. Args is the
// method-specific payload (may be empty). Correlated by Envelope.Corr.
type ControlRequest struct {
	Method string          `json:"method"`
	Args   json.RawMessage `json:"args,omitempty"`
}

// ControlResponse is the body of a MsgControlResponse. Error is "" on
// success; a non-empty Error means the call failed and Result is
// undefined (mirrors the agent methods that return an error).
type ControlResponse struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// ControlName is the args payload for single-string methods.
type ControlName struct {
	Name string `json:"name"`
}

// ControlNames is the args payload for string-slice methods.
type ControlNames struct {
	Names []string `json:"names"`
}

// ---- capability/credential broker --------------------------------
//
// The worker is network-sealed under the sandboxed backend and runs
// with an empty environment, so anything it legitimately needs that
// would otherwise be a host secret or an outbound connection is
// requested at runtime over this leg. The host brokers it; the policy
// default is DENY (no grant unless something host-side explicitly
// authorizes it). Correlated by Envelope.Corr like the other RPC legs.

const (
	CapCredential = "credential" // Name is a logical secret id; Grant.Secret carries the value
	CapEgress     = "egress"     // Name is "host:port"; a one-off outbound allowance
)

// CapabilityRequest is the body of a MsgCapabilityRequest.
type CapabilityRequest struct {
	Type   string `json:"type"`             // CapCredential | CapEgress
	Name   string `json:"name"`             // credential id or host:port
	Reason string `json:"reason,omitempty"` // worker-supplied justification (audit/UI)
}

// CapabilityGrant is the body of a MsgCapabilityGrant. Granted=false is
// the default-deny answer; Reason explains it. Secret is set only for a
// granted CapCredential. TTLSeconds is advisory ("scoped, preferably
// short-lived") — 0 means unspecified.
type CapabilityGrant struct {
	Granted    bool   `json:"granted"`
	Secret     string `json:"secret,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// CompactPreview mirrors agent.CompactPreviewResult (plain scalars; the
// worker adapter converts to/from the agent type so wire stays
// agent-free).
type CompactPreview struct {
	BeforeTokens        int  `json:"before_tokens"`
	EstAfterTokens      int  `json:"est_after_tokens"`
	MessagesToSummarise int  `json:"messages_to_summarise"`
	NothingToDo         bool `json:"nothing_to_do"`
}

// ForceCompactResult carries agent.ForceCompact's bool; its error rides
// ControlResponse.Error.
type ForceCompactResult struct {
	Did bool `json:"did"`
}

// ForcePruneResult mirrors agent.ForcePrune's (stubbed, before, after).
type ForcePruneResult struct {
	Stubbed int `json:"stubbed"`
	Before  int `json:"before"`
	After   int `json:"after"`
}

// PrefixBreakdown mirrors agent.PrefixBreakdown.
type PrefixBreakdown struct {
	Total        int `json:"total"`
	System       int `json:"system"`
	Pinned       int `json:"pinned"`
	ToolActive   int `json:"tool_active"`
	ToolStubbed  int `json:"tool_stubbed"`
	Conversation int `json:"conversation"`
}
