// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"strings"
	"testing"
)

type fakeCheckpointer struct {
	called bool
	reason string
}

func (f *fakeCheckpointer) RequestCheckpoint(reason string) {
	f.called = true
	f.reason = reason
}

func TestCheckpointTool_CallsRequester(t *testing.T) {
	fc := &fakeCheckpointer{}
	ac := &AgentContext{Checkpoint: fc}

	r, err := CheckpointTool{}.Run(context.Background(),
		map[string]any{"reason": "finished step 1"}, ac)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !fc.called {
		t.Fatal("requester not called")
	}
	if fc.reason != "finished step 1" {
		t.Errorf("reason=%q, want %q", fc.reason, "finished step 1")
	}
	if !strings.Contains(r.LLMOutput, "finished step 1") {
		t.Errorf("LLMOutput should mention the reason, got: %q", r.LLMOutput)
	}
}

func TestCheckpointTool_TrimsWhitespace(t *testing.T) {
	fc := &fakeCheckpointer{}
	ac := &AgentContext{Checkpoint: fc}

	_, _ = CheckpointTool{}.Run(context.Background(),
		map[string]any{"reason": "  done  "}, ac)
	if fc.reason != "done" {
		t.Errorf("reason=%q, want %q", fc.reason, "done")
	}
}

func TestCheckpointTool_NoRequesterWired(t *testing.T) {
	// nil Checkpoint should produce a tool-result string explaining
	// the no-op instead of panicking. Defensive — the in-process path
	// always wires it.
	ac := &AgentContext{}
	r, err := CheckpointTool{}.Run(context.Background(),
		map[string]any{"reason": "x"}, ac)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(r.LLMOutput, "not queued") {
		t.Errorf("expected diagnostic in LLMOutput, got: %q", r.LLMOutput)
	}
}

func TestCheckpointTool_RegisteredByDefault(t *testing.T) {
	r := BuildDefault()
	if r.Get("checkpoint") == nil {
		t.Fatal("checkpoint tool not registered in BuildDefault")
	}
}
