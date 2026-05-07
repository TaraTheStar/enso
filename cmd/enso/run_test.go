// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/TaraTheStar/enso/internal/bus"
)

// TestRenderJSON drives renderJSONTo with a synthetic event stream and checks
// the line-delimited JSON shape, payload pass-through, and the
// sawToolError side-effect on errored / denied tool calls.
func TestRenderJSON(t *testing.T) {
	ch := make(chan bus.Event, 16)
	for _, e := range []bus.Event{
		{Type: bus.EventUserMessage, Payload: "hi"},
		{Type: bus.EventAssistantDelta, Payload: "hel"},
		{Type: bus.EventAssistantDelta, Payload: "lo"},
		{Type: bus.EventAssistantDone},
		{Type: bus.EventToolCallStart, Payload: map[string]any{"name": "bash", "id": "c1", "args": map[string]any{"cmd": "ls"}}},
		{Type: bus.EventToolCallEnd, Payload: map[string]any{"name": "bash", "id": "c1", "result": "ok", "error": nil}},
		{Type: bus.EventToolCallEnd, Payload: map[string]any{"name": "bash", "id": "c2", "error": errors.New("boom")}},
		{Type: bus.EventToolCallEnd, Payload: map[string]any{"name": "bash", "id": "c3", "denied": true}},
		{Type: bus.EventCompacted, Payload: map[string]any{"reason": "60%", "summary": "..."}},
		{Type: bus.EventCancelled},
		{Type: bus.EventError, Payload: errors.New("nope")},
	} {
		ch <- e
	}
	close(ch)

	var buf bytes.Buffer
	var sawErr bool
	renderJSONTo(&buf, ch, &sawErr)

	if !sawErr {
		t.Fatalf("sawToolError should be true after errored or denied tool_call_end")
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 11 {
		t.Fatalf("expected 11 JSON lines, got %d:\n%s", len(lines), buf.String())
	}

	for i, line := range lines {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d not valid JSON: %v\n%s", i, err, line)
		}
		if _, ok := m["type"].(string); !ok {
			t.Fatalf("line %d missing string field 'type'", i)
		}
	}

	// Spot-check a couple of fields that a consumer would care about.
	var errLine map[string]any
	if err := json.Unmarshal([]byte(lines[6]), &errLine); err != nil {
		t.Fatal(err)
	}
	if errLine["type"] != "tool_call_end" || errLine["error"] != "boom" {
		t.Fatalf("expected error pass-through, got %v", errLine)
	}

	var denyLine map[string]any
	if err := json.Unmarshal([]byte(lines[7]), &denyLine); err != nil {
		t.Fatal(err)
	}
	if denyLine["denied"] != true {
		t.Fatalf("expected denied=true, got %v", denyLine)
	}
}
