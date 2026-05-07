// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestMessageMarshal_ToolMessageWithEmptyContent locks in the fix for
// the HTTP 400 surfaced by the eval harness: tool result messages must
// carry a `content` field even when empty, because some OpenAI-compatible
// servers reject requests where non-assistant messages drop content.
func TestMessageMarshal_ToolMessageWithEmptyContent(t *testing.T) {
	m := Message{Role: "tool", ToolCallID: "c1", Name: "bash", Content: ""}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"content":""`) {
		t.Errorf("tool message must emit content even when empty; got %s", b)
	}
}

func TestMessageMarshal_UserEmptyContent(t *testing.T) {
	m := Message{Role: "user", Content: ""}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"content":""`) {
		t.Errorf("user message must emit content even when empty; got %s", b)
	}
}

// TestMessageMarshal_AssistantWithToolCallsOmitsContent — the spec's
// only ergonomic concession: an assistant message that delegates to tool
// calls may omit content. We follow that to avoid sending an empty
// string where the model didn't speak.
func TestMessageMarshal_AssistantWithToolCallsOmitsContent(t *testing.T) {
	m := Message{
		Role: "assistant",
		ToolCalls: []ToolCall{{
			ID:   "c1",
			Type: "function",
		}},
	}
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), `"content"`) {
		t.Errorf("assistant-with-tool_calls should omit content; got %s", b)
	}
}

// TestMessageMarshal_AssistantTextOnly — when an assistant message has
// text but no tool calls, content is sent (even if empty).
func TestMessageMarshal_AssistantTextOnly(t *testing.T) {
	m := Message{Role: "assistant", Content: "ok"}
	b, _ := json.Marshal(m)
	if !strings.Contains(string(b), `"content":"ok"`) {
		t.Errorf("assistant-text content missing; got %s", b)
	}
}

func TestMessageRoundTrip(t *testing.T) {
	in := Message{
		Role:       "tool",
		Content:    "(exit 0, no output)",
		ToolCallID: "c1",
		Name:       "bash",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out Message
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("roundtrip mismatch:\nin:  %+v\nout: %+v", in, out)
	}
}
