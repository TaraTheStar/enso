// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
)

// TestPromptRequest_WireFormSafe is the integration check for bus.WireForm
// against the real *PromptRequest type. The two json:"-" fields (Respond
// channel and Deadline) must not appear in the serialised payload so
// daemon-socket observers never see internal bookkeeping that doesn't
// belong on the wire. Lives in the permissions package to avoid the
// import cycle the bus package would hit if it referenced this type
// directly.
func TestPromptRequest_WireFormSafe(t *testing.T) {
	req := &PromptRequest{
		ToolName:  "bash",
		ArgString: "ls -la",
		Args:      map[string]any{"cmd": "ls -la"},
		Diff:      "+ new line",
		AgentID:   "agent-3",
		AgentRole: "reviewer",
		Respond:   make(chan Decision, 1),
		Deadline:  time.Now().Add(30 * time.Second),
	}

	typ, raw, ok := bus.Event{Type: bus.EventPermissionRequest, Payload: req}.WireForm()
	if !ok {
		t.Fatal("WireForm returned ok=false for PermissionRequest")
	}
	if typ != "PermissionRequest" {
		t.Errorf("typ = %q, want PermissionRequest", typ)
	}

	// Decode the wire payload and check each expected key is present
	// and each forbidden one is absent.
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal wire payload: %v (raw=%s)", err, raw)
	}
	mustHave := map[string]string{
		"tool_name":  "bash",
		"arg_string": "ls -la",
		"diff":       "+ new line",
		"agent_id":   "agent-3",
		"agent_role": "reviewer",
	}
	for k, want := range mustHave {
		got, ok := decoded[k].(string)
		if !ok || got != want {
			t.Errorf("payload[%q] = %v, want %q", k, decoded[k], want)
		}
	}
	if _, present := decoded["args"]; !present {
		t.Errorf("payload missing args: %s", raw)
	}

	// json.Marshal lowercases nothing; check case-insensitively to
	// catch either accidental capitalisation or accidental marshalling.
	low := strings.ToLower(string(raw))
	if strings.Contains(low, "respond") {
		t.Errorf("Respond channel leaked into wire payload: %s", raw)
	}
	if strings.Contains(low, "deadline") {
		t.Errorf("Deadline leaked into wire payload: %s", raw)
	}
}
