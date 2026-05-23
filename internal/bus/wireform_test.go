// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"errors"
	"strings"
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
	// wire events. EventPermissionRequest IS now wire-safe (see
	// TestWireFormPermissionRequest) because the payload's channel and
	// deadline are json:"-" — observers get the safe shape.
	for _, et := range []EventType{
		EventPermissionResponse, EventPermissionAuto,
	} {
		if _, _, ok := (Event{Type: et}).WireForm(); ok {
			t.Fatalf("event %d should be dropped from the wire", et)
		}
	}
}

// TestWireFormPermissionRequest checks that EventPermissionRequest
// carries a sanitised payload across the wire: tool_name, args, and
// agent identifiers survive; the live Respond channel and Deadline
// (both json:"-" on permissions.PromptRequest) are stripped.
//
// Uses an inline struct that mirrors PromptRequest's json shape rather
// than importing internal/permissions (which would create a cycle —
// permissions imports bus). The integration test in
// internal/permissions/wireform_test.go runs the same assertion against
// the real type.
func TestWireFormPermissionRequest(t *testing.T) {
	type promptLike struct {
		ToolName  string         `json:"tool_name"`
		ArgString string         `json:"arg_string,omitempty"`
		Args      map[string]any `json:"args,omitempty"`
		AgentID   string         `json:"agent_id,omitempty"`
		Respond   chan struct{}  `json:"-"`
	}
	payload := &promptLike{
		ToolName: "bash",
		Args:     map[string]any{"cmd": "ls"},
		AgentID:  "agent-7",
		Respond:  make(chan struct{}),
	}

	typ, raw, ok := Event{Type: EventPermissionRequest, Payload: payload}.WireForm()
	if !ok || typ != "PermissionRequest" {
		t.Fatalf("WireForm: ok=%v typ=%q", ok, typ)
	}
	got := string(raw)
	if !strings.Contains(got, `"tool_name":"bash"`) {
		t.Errorf("payload missing tool_name: %s", got)
	}
	if !strings.Contains(got, `"agent_id":"agent-7"`) {
		t.Errorf("payload missing agent_id: %s", got)
	}
	if strings.Contains(got, "Respond") || strings.Contains(got, "respond") {
		t.Errorf("Respond channel leaked into wire payload: %s", got)
	}

	// FromWire returns ok=false on purpose (see comment in bus.go) —
	// in-process consumers reach permissions via a separate path.
	if _, ok := FromWire("PermissionRequest", raw); ok {
		t.Errorf("FromWire should return ok=false for PermissionRequest (see asymmetry comment)")
	}
}
