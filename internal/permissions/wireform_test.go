// SPDX-License-Identifier: AGPL-3.0-or-later

package permissions

import (
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
)

// TestPromptRequest_WireFormSafe is the integration check for bus.WireForm
// against the real *PromptRequest type. EventPermissionRequest is not
// wire-serializable: it carries a live Respond channel and lacks the
// RequestID a client needs to answer. A generic wire fan-out would render
// an un-answerable phantom prompt that replays on every reconnect, so
// WireForm must return ok=false. Permission requests reach clients through
// a dedicated, answerable path instead (daemon proxyPermission /
// Backend MsgPermissionRequest). Lives in the permissions package to
// exercise the real type, which the bus package can't import (cycle).
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

	if _, _, ok := (bus.Event{Type: bus.EventPermissionRequest, Payload: req}).WireForm(); ok {
		t.Fatal("WireForm should return ok=false for EventPermissionRequest")
	}
}
