// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"errors"
	"testing"
)

func TestWireFormSerializableEvents(t *testing.T) {
	typ, payload, ok := Event{Type: EventAssistantDelta, Payload: "hi"}.WireForm()
	if !ok || typ != "AssistantDelta" || string(payload) != `"hi"` {
		t.Fatalf("AssistantDelta: ok=%v typ=%q payload=%s", ok, typ, payload)
	}

	// ReasoningDelta crosses the seam (the worker's agent is the only
	// reasoning source once run/TUI are behind the Backend) and round-trips.
	typ, payload, ok = Event{Type: EventReasoningDelta, Payload: "thinking"}.WireForm()
	if !ok || typ != "ReasoningDelta" || string(payload) != `"thinking"` {
		t.Fatalf("ReasoningDelta: ok=%v typ=%q payload=%s", ok, typ, payload)
	}
	if ev, ok := FromWire("ReasoningDelta", payload); !ok ||
		ev.Type != EventReasoningDelta || ev.Payload.(string) != "thinking" {
		t.Fatalf("ReasoningDelta round-trip: ok=%v ev=%+v", ok, ev)
	}

	// errors must be coerced to their string, not dropped.
	_, payload, ok = Event{Type: EventError, Payload: errors.New("boom")}.WireForm()
	if !ok || string(payload) != `"boom"` {
		t.Fatalf("Error payload not simplified: ok=%v payload=%s", ok, payload)
	}

	// nested map with an error value is simplified recursively.
	_, payload, ok = Event{Type: EventToolCallEnd, Payload: map[string]any{
		"name": "bash", "error": errors.New("x"),
	}}.WireForm()
	if !ok || (string(payload) != `{"error":"x","name":"bash"}` &&
		string(payload) != `{"name":"bash","error":"x"}`) {
		t.Fatalf("ToolCallEnd map not simplified: %s", payload)
	}
}

func TestWireFormDropsInternalEvents(t *testing.T) {
	// PermissionResponse / PermissionAuto are decision-feedback events
	// that travel host-locally only; they must never cross as plain
	// wire events. EventPermissionRequest is also dropped: it carries a
	// live Respond channel and lacks the RequestID a client needs to
	// answer, so a generic wire fan-out would render an un-answerable
	// phantom prompt. Both the daemon and the Backend worker proxy it
	// through a dedicated, answerable path instead.
	for _, et := range []EventType{
		EventPermissionResponse, EventPermissionAuto, EventPermissionRequest,
	} {
		if _, _, ok := (Event{Type: et}).WireForm(); ok {
			t.Fatalf("event %d should be dropped from the wire", et)
		}
	}
}

// TestWireFormPermissionRequest checks that EventPermissionRequest is
// not wire-serializable via the generic path: WireForm returns ok=false
// so it never reaches a daemon/worker fan-out. Permission requests are
// proxied through a dedicated, answerable control path instead.
func TestWireFormPermissionRequest(t *testing.T) {
	type promptLike struct {
		ToolName string        `json:"tool_name"`
		AgentID  string        `json:"agent_id,omitempty"`
		Respond  chan struct{} `json:"-"`
	}
	payload := &promptLike{
		ToolName: "bash",
		AgentID:  "agent-7",
		Respond:  make(chan struct{}),
	}

	if _, _, ok := (Event{Type: EventPermissionRequest, Payload: payload}).WireForm(); ok {
		t.Fatal("WireForm should return ok=false for EventPermissionRequest")
	}
}
