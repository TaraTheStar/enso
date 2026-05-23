// SPDX-License-Identifier: AGPL-3.0-or-later

package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
)

// TestOnEvent_PipesJSONOnStdin is the headline contract: the hook
// command receives one JSON object on stdin per fired event, with
// session_id/cwd/type/payload populated. Captures stdin via `cat`
// redirected to a file because that's the most realistic exercise of
// the pipe-and-wait machinery.
func TestOnEvent_PipesJSONOnStdin(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "stdin.json")

	h := New(Config{
		OnEvent: "cat > " + shellQuote(out),
	})
	h.OnEvent(tmp, "sess-1", bus.Event{
		Type:    bus.EventUserMessage,
		Payload: "hello",
	})

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("hook didn't pipe to stdin: %v", err)
	}
	var got struct {
		SessionID string          `json:"session_id"`
		Cwd       string          `json:"cwd"`
		Type      string          `json:"type"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("hook stdin not valid JSON: %v\n%s", err, data)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("session_id = %q, want sess-1", got.SessionID)
	}
	if got.Cwd != tmp {
		t.Errorf("cwd = %q, want %q", got.Cwd, tmp)
	}
	if got.Type != "UserMessage" {
		t.Errorf("type = %q, want UserMessage", got.Type)
	}
	if string(got.Payload) != `"hello"` {
		t.Errorf("payload = %s, want %q", got.Payload, `"hello"`)
	}
}

// TestOnEvent_FilterDefaultExcludesDeltas guards the most important
// performance invariant: per-token AssistantDelta events MUST NOT
// trigger a hook spawn by default. A test session might emit tens of
// thousands; spawning a subprocess per token would be catastrophic.
func TestOnEvent_FilterDefaultExcludesDeltas(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "called")

	h := New(Config{
		OnEvent: "touch " + shellQuote(out),
	})
	// Default filter is in effect (no OnEvents). AssistantDelta and
	// ReasoningDelta must be skipped.
	h.OnEvent(tmp, "s", bus.Event{Type: bus.EventAssistantDelta, Payload: "tok"})
	h.OnEvent(tmp, "s", bus.Event{Type: bus.EventReasoningDelta, Payload: "tok"})

	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("default filter fired hook for delta event")
	}

	// Sanity: an in-filter event DOES fire so the filter isn't just
	// "always reject."
	h.OnEvent(tmp, "s", bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{"name": "bash"}})
	if _, err := os.Stat(out); err != nil {
		t.Errorf("ToolCallStart should have fired hook: %v", err)
	}
}

// TestOnEvent_ExplicitFilterOverridesDefault confirms a user-supplied
// OnEvents list wins entirely — no implicit merge with the defaults.
// Listing one type means "I want only that type."
func TestOnEvent_ExplicitFilterOverridesDefault(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "log")

	h := New(Config{
		OnEvent:  "cat >> " + shellQuote(out),
		OnEvents: []string{"AssistantDelta"}, // ONLY deltas
	})
	// AssistantDelta is normally excluded; explicit list keeps it.
	h.OnEvent(tmp, "s", bus.Event{Type: bus.EventAssistantDelta, Payload: "x"})
	// ToolCallStart is in the default but NOT in this explicit list.
	h.OnEvent(tmp, "s", bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{}})

	data, _ := os.ReadFile(out)
	if !strings.Contains(string(data), `"AssistantDelta"`) {
		t.Errorf("delta should have fired: %s", data)
	}
	if strings.Contains(string(data), `"ToolCallStart"`) {
		t.Errorf("ToolCallStart leaked past explicit filter: %s", data)
	}
}

// TestOnEvent_UnserializableEventsSkipped covers events that
// bus.Event.WireForm rejects (PermissionResponse / PermissionAuto —
// host-only feedback channels). They must NOT fire the hook.
func TestOnEvent_UnserializableEventsSkipped(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "ran")
	h := New(Config{
		OnEvent: "touch " + shellQuote(out),
	})
	h.OnEvent(tmp, "s", bus.Event{Type: bus.EventPermissionResponse})
	h.OnEvent(tmp, "s", bus.Event{Type: bus.EventPermissionAuto})
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("hook fired for unserializable event")
	}
}

// TestOnEvent_EnvIsMerged verifies that hook-config Env entries reach
// the subprocess and override matching os.Environ() values. Done by
// having the hook echo $WATCHOURAI_TOKEN to a file.
func TestOnEvent_EnvIsMerged(t *testing.T) {
	tmp := t.TempDir()
	out := filepath.Join(tmp, "env-out")
	t.Setenv("WATCHOURAI_TOKEN", "shell-rc-value")

	h := New(Config{
		OnEvent: `printf %s "$WATCHOURAI_TOKEN" > ` + shellQuote(out),
		Env:     map[string]string{"WATCHOURAI_TOKEN": "config-overrides"},
	})
	h.OnEvent(tmp, "s", bus.Event{Type: bus.EventUserMessage, Payload: "hi"})

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("hook didn't run: %v", err)
	}
	if string(data) != "config-overrides" {
		t.Errorf("env merge: got %q, want config-overrides", data)
	}
}

// TestOnEvent_EmptyCmdNoOp confirms the disabled path: an empty
// OnEvent command must not spawn any process or fire any warn.
func TestOnEvent_EmptyCmdNoOp(t *testing.T) {
	h, msgs, mu := recordingHooks("", "")
	h.OnEvent(t.TempDir(), "s", bus.Event{Type: bus.EventToolCallStart})
	mu.Lock()
	defer mu.Unlock()
	if len(*msgs) != 0 {
		t.Errorf("empty OnEvent should be silent, got %v", *msgs)
	}
}

// TestOnEvent_NilReceiverSafe pins the nil-Hooks contract so a caller
// that constructs a sub-agent without hooks can't crash on event fanout.
func TestOnEvent_NilReceiverSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("nil *Hooks panicked: %v", r)
		}
	}()
	var h *Hooks
	h.OnEvent("/", "s", bus.Event{Type: bus.EventToolCallStart})
}

// TestOnEvent_TimeoutWarns shares the same posture as OnFileEdit:
// a slow command is killed at h.Timeout and surfaces a Warn so the
// operator can spot a hung dispatch.
func TestOnEvent_TimeoutWarns(t *testing.T) {
	h, msgs, mu := recordingHooks("", "")
	h.OnEventCmd = "sleep 5"
	h.Timeout = 100 * time.Millisecond
	start := time.Now()
	h.OnEvent(t.TempDir(), "s", bus.Event{Type: bus.EventToolCallStart})
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Errorf("timeout did not kill the process: elapsed=%v", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, m := range *msgs {
		if strings.Contains(m, "timed out") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected timeout warn, got %v", *msgs)
	}
}

// TestDefaultEventFilter_SanityChecks the documented default set: it
// must include the lifecycle staples (start/idle/end), at least one
// tool-call event, and exclude the per-token deltas. Catches drift if
// someone adds an event type and accidentally puts it in the default.
func TestDefaultEventFilter_SanityChecks(t *testing.T) {
	must := []string{
		"UserMessage", "AgentStart", "AgentIdle", "AgentEnd",
		"ToolCallStart", "ToolCallEnd", "Cancelled", "Error",
		"PermissionRequest",
	}
	for _, name := range must {
		if !slices.Contains(DefaultEventFilter, name) {
			t.Errorf("DefaultEventFilter missing %q", name)
		}
	}
	mustNot := []string{"AssistantDelta", "ReasoningDelta", "ToolCallProgress"}
	for _, name := range mustNot {
		if slices.Contains(DefaultEventFilter, name) {
			t.Errorf("DefaultEventFilter contains chatty event %q", name)
		}
	}
}
