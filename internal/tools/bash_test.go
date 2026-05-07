// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
)

// TestBashTool_PublishesProgress runs a tiny shell command and asserts that
// ToolCallProgress events flow through the bus tagged with the right id and
// containing the command's stdout. Smoke-level — depends on `sh` and `echo`
// being available, which is true in any reasonable Go test environment.
func TestBashTool_PublishesProgress(t *testing.T) {
	b := bus.New()
	sub := b.Subscribe(16)

	ac := &AgentContext{
		Cwd:           t.TempDir(),
		Bus:           b,
		CurrentToolID: "tc-1",
	}

	res, err := BashTool{}.Run(
		context.Background(),
		map[string]interface{}{"cmd": "echo hello"},
		ac,
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "hello") {
		t.Errorf("LLMOutput = %q, expected to contain 'hello'", res.LLMOutput)
	}

	// Drain bus until we either find the expected event or time out.
	deadline := time.After(time.Second)
	gotProgress := false
	for !gotProgress {
		select {
		case evt := <-sub:
			if evt.Type != bus.EventToolCallProgress {
				continue
			}
			m, _ := evt.Payload.(map[string]any)
			if m["id"] == "tc-1" && strings.Contains(m["text"].(string), "hello") {
				gotProgress = true
			}
		case <-deadline:
			t.Fatalf("no ToolCallProgress event with id=tc-1 / text~hello within timeout")
		}
	}
}

// TestBashTool_EmptyStdoutIsExplicit covers the case that surfaced via
// the eval harness: a command that succeeds with no output. An empty
// LLMOutput would marshal to a tool message with no `content` field
// (omitempty), and some OpenAI-compatible servers reject that with HTTP
// 400. We substitute an explicit "(exit 0, no output)" marker.
func TestBashTool_EmptyStdoutIsExplicit(t *testing.T) {
	ac := &AgentContext{Cwd: t.TempDir(), Bus: bus.New()}
	res, err := BashTool{}.Run(
		context.Background(),
		map[string]interface{}{"cmd": "true"}, // exits 0 with no stdout
		ac,
	)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.LLMOutput == "" {
		t.Errorf("empty stdout must yield non-empty LLMOutput so tool messages carry content")
	}
	if !strings.Contains(res.LLMOutput, "no output") {
		t.Errorf("expected explicit marker, got %q", res.LLMOutput)
	}
}
