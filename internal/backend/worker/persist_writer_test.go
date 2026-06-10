// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/llm"
)

// These tests cover the worker leg of isolated-mode session persistence:
// remoteWriter must turn each tools.SessionWriter call into exactly the
// MsgPersist* envelope the host loop applies to the host DB. For
// podman/lima sessions this is the ONLY route session data reaches the
// host store — a silent regression here produces unresumable sessions.

// TestRemoteWriter_PersistEnvelopes drives all three writer methods over
// a real channel pair and asserts the envelope kinds, bodies, ordering
// and the worker-side seq mirror.
func TestRemoteWriter_PersistEnvelopes(t *testing.T) {
	workerCh, hostCh := newChannelPair()
	rw := &remoteWriter{s: &seam{ch: workerCh}, sessionID: "sess-1"}

	if got := rw.SessionID(); got != "sess-1" {
		t.Fatalf("SessionID() = %q, want %q", got, "sess-1")
	}

	// Collector: pipe sends block until read, so drain concurrently and
	// assert from the buffered stream afterwards.
	envs := make(chan backend.Envelope, 16)
	go func() {
		for {
			e, err := hostCh.Recv()
			if err != nil {
				close(envs)
				return
			}
			envs <- e
		}
	}()
	next := func(wantKind backend.MsgKind) backend.Envelope {
		t.Helper()
		select {
		case e, ok := <-envs:
			if !ok {
				t.Fatal("channel closed before expected envelope")
			}
			if e.Kind != wantKind {
				t.Fatalf("envelope kind = %q, want %q", e.Kind, wantKind)
			}
			return e
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for %q envelope", wantKind)
			return backend.Envelope{}
		}
	}

	// 1) Plain user message → MsgPersistMessage, seq 1.
	seq1, err := rw.AppendMessage(llm.Message{Role: "user", Content: "hi"}, "")
	if err != nil {
		t.Fatalf("AppendMessage: %v", err)
	}
	if seq1 != 1 {
		t.Fatalf("first AppendMessage seq = %d, want 1", seq1)
	}
	var pm wire.PersistMessage
	if err := json.Unmarshal(next(backend.MsgPersistMessage).Body, &pm); err != nil {
		t.Fatalf("decode persist message: %v", err)
	}
	if pm.Msg.Role != "user" || pm.Msg.Content != "hi" || pm.AgentID != "" {
		t.Fatalf("persist message body = %+v, want user/hi/top-level", pm)
	}

	// 2) Assistant message with chain-of-thought: llm.Message.Reasoning
	// is `json:"-"` so it must ride the EXPLICIT wire field, or it would
	// silently vanish from the host DB in isolated mode.
	asst := llm.Message{Role: "assistant", Content: "hello"}
	asst.Reasoning = "chain-of-thought"
	seq2, err := rw.AppendMessage(asst, "sub1")
	if err != nil {
		t.Fatalf("AppendMessage assistant: %v", err)
	}
	if seq2 != 2 {
		t.Fatalf("second AppendMessage seq = %d, want 2", seq2)
	}
	if err := json.Unmarshal(next(backend.MsgPersistMessage).Body, &pm); err != nil {
		t.Fatalf("decode persist message: %v", err)
	}
	if pm.AgentID != "sub1" {
		t.Fatalf("agent attribution lost: AgentID = %q, want sub1", pm.AgentID)
	}
	if pm.Reasoning != "chain-of-thought" {
		t.Fatalf("Reasoning did not ride the explicit wire field: %q", pm.Reasoning)
	}

	// 3) Usage → MsgPersistMessageUsage. seq is deliberately not shipped
	// (the host attributes to its own last-applied message), so the body
	// carries usage + agent only.
	usage := llm.MessageUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	if err := rw.AppendMessageUsage(seq2, usage, "sub1"); err != nil {
		t.Fatalf("AppendMessageUsage: %v", err)
	}
	var pu wire.PersistMessageUsage
	if err := json.Unmarshal(next(backend.MsgPersistMessageUsage).Body, &pu); err != nil {
		t.Fatalf("decode persist usage: %v", err)
	}
	if pu.Usage != usage || pu.AgentID != "sub1" {
		t.Fatalf("persist usage body = %+v, want %+v for sub1", pu, usage)
	}

	// 4) Tool call → MsgPersistToolCall with every field intact.
	if err := rw.AppendToolCall("c1", "bash", map[string]any{"cmd": "ls"}, "out", "full", "ok"); err != nil {
		t.Fatalf("AppendToolCall: %v", err)
	}
	var pt wire.PersistToolCall
	if err := json.Unmarshal(next(backend.MsgPersistToolCall).Body, &pt); err != nil {
		t.Fatalf("decode persist tool call: %v", err)
	}
	if pt.CallID != "c1" || pt.Name != "bash" || pt.LLMOutput != "out" ||
		pt.FullOutput != "full" || pt.Status != "ok" {
		t.Fatalf("persist tool call body = %+v", pt)
	}
	if got, _ := pt.Args["cmd"].(string); got != "ls" {
		t.Fatalf("tool args[cmd] = %q, want ls", got)
	}
}

// TestRemoteWriter_SendFailureSurfacesError: when the Channel is dead
// every append must return an error (AppendMessage with seq 0) so the
// agent loop sees the persistence failure instead of silently dropping
// session rows.
func TestRemoteWriter_SendFailureSurfacesError(t *testing.T) {
	rw := &remoteWriter{s: &seam{ch: failSendChannel{}}, sessionID: "sess-dead"}

	seq, err := rw.AppendMessage(llm.Message{Role: "user", Content: "hi"}, "")
	if err == nil {
		t.Fatal("AppendMessage on a dead channel must error")
	}
	if seq != 0 {
		t.Fatalf("failed AppendMessage seq = %d, want 0", seq)
	}
	if err := rw.AppendMessageUsage(1, llm.MessageUsage{TotalTokens: 1}, ""); err == nil {
		t.Fatal("AppendMessageUsage on a dead channel must error")
	}
	if err := rw.AppendToolCall("c1", "bash", nil, "", "", "ok"); err == nil {
		t.Fatal("AppendToolCall on a dead channel must error")
	}
}
