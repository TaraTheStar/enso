// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/llm"
)

// TestComputeStats inserts two sessions worth of messages and tool calls into
// a temp store, then asserts the aggregate buckets line up. Covers per-role
// message counts, model grouping, tool status splits, and approx-token sum.
func TestComputeStats(t *testing.T) {
	tmp := t.TempDir()
	s, err := OpenAt(filepath.Join(tmp, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	w1, err := NewSession(s, "qwen3.6", "local", "/p1")
	if err != nil {
		t.Fatal(err)
	}
	if err := w1.AppendMessage(llm.Message{Role: "user", Content: "hello"}, ""); err != nil {
		t.Fatal(err)
	}
	asst := llm.Message{Role: "assistant", Content: "world"}
	asst.ToolCalls = []llm.ToolCall{{ID: "c1"}}
	asst.ToolCalls[0].Function.Name = "bash"
	if err := w1.AppendMessage(asst, ""); err != nil {
		t.Fatal(err)
	}
	if err := w1.AppendToolCall("c1", "bash", map[string]any{"cmd": "ls"}, "ok", "ok", "ok"); err != nil {
		t.Fatal(err)
	}
	if err := w1.AppendToolCall("c2", "bash", map[string]any{"cmd": "rm -rf /"}, "denied", "", "denied"); err != nil {
		t.Fatal(err)
	}

	w2, err := NewSession(s, "other-model", "local", "/p2")
	if err != nil {
		t.Fatal(err)
	}
	if err := w2.AppendMessage(llm.Message{Role: "user", Content: "hi"}, ""); err != nil {
		t.Fatal(err)
	}
	if err := w2.AppendToolCall("c3", "read", map[string]any{}, "boom", "boom", "error"); err != nil {
		t.Fatal(err)
	}

	st, err := ComputeStats(s, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	if st.SessionCount != 2 {
		t.Errorf("SessionCount = %d, want 2", st.SessionCount)
	}
	if st.MessagesByRole["user"] != 2 {
		t.Errorf("user messages = %d, want 2", st.MessagesByRole["user"])
	}
	if st.MessagesByRole["assistant"] != 1 {
		t.Errorf("assistant messages = %d, want 1", st.MessagesByRole["assistant"])
	}
	if st.SessionsByModel["qwen3.6"] != 1 || st.SessionsByModel["other-model"] != 1 {
		t.Errorf("model counts wrong: %+v", st.SessionsByModel)
	}

	bash := st.ToolCallsByName["bash"]
	if bash.Total != 2 || bash.OK != 1 || bash.Denied != 1 {
		t.Errorf("bash stats = %+v, want {Total:2 OK:1 Denied:1}", bash)
	}
	read := st.ToolCallsByName["read"]
	if read.Total != 1 || read.Error != 1 {
		t.Errorf("read stats = %+v, want {Total:1 Error:1}", read)
	}

	if st.ApproxTotalTokens <= 0 {
		t.Errorf("ApproxTotalTokens = %d, want >0", st.ApproxTotalTokens)
	}
}
