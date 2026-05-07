// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/tools"
)

func TestSpawn_RejectsAtMaxDepth(t *testing.T) {
	ac := &tools.AgentContext{
		Bus:          bus.New(),
		Depth:        3, // already at max
		MaxDepth:     3,
		MaxAgents:    16,
		GlobalAgents: &atomic.Int64{},
	}
	res, err := SpawnTool{}.Run(context.Background(),
		map[string]interface{}{"prompt": "do work"}, ac)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "max recursion depth") {
		t.Errorf("output = %q, want depth-rejected message", res.LLMOutput)
	}
	// Counter should not have leaked: rejection happens before increment.
	if got := ac.GlobalAgents.Load(); got != 0 {
		t.Errorf("GlobalAgents = %d, want 0 (no spawn attempted)", got)
	}
}

func TestSpawn_RejectsAtMaxAgents(t *testing.T) {
	gc := &atomic.Int64{}
	gc.Store(16) // already at limit; the increment will land at 17

	ac := &tools.AgentContext{
		Bus:          bus.New(),
		Depth:        0,
		MaxDepth:     3,
		MaxAgents:    16,
		GlobalAgents: gc,
	}
	res, err := SpawnTool{}.Run(context.Background(),
		map[string]interface{}{"prompt": "do work"}, ac)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "max global agents") {
		t.Errorf("output = %q, want global-agents-rejected message", res.LLMOutput)
	}
	// Counter must be decremented after rejection so it doesn't leak.
	if got := gc.Load(); got != 16 {
		t.Errorf("GlobalAgents = %d, want 16 (rejected spawn must not retain its slot)", got)
	}
}

func TestSpawn_RequiresPrompt(t *testing.T) {
	ac := &tools.AgentContext{
		Bus:          bus.New(),
		MaxDepth:     3,
		MaxAgents:    16,
		GlobalAgents: &atomic.Int64{},
	}
	_, err := SpawnTool{}.Run(context.Background(),
		map[string]interface{}{}, ac)
	if err == nil {
		t.Errorf("missing prompt: want error")
	}
}

func TestSpawn_RequiresGlobalAgentsCounter(t *testing.T) {
	ac := &tools.AgentContext{
		Bus:      bus.New(),
		MaxDepth: 3,
		// GlobalAgents is nil — must not panic
	}
	_, err := SpawnTool{}.Run(context.Background(),
		map[string]interface{}{"prompt": "x"}, ac)
	if err == nil {
		t.Errorf("nil GlobalAgents: want error, not panic")
	}
}

func TestFilterRegistry_PicksNamedToolsOnly(t *testing.T) {
	parent := tools.BuildDefault()
	child := parent.Filter(asStringSlice([]interface{}{"read", "grep", "nonexistent"}))

	if got := child.Get("read"); got == nil {
		t.Errorf("child should have read")
	}
	if got := child.Get("grep"); got == nil {
		t.Errorf("child should have grep")
	}
	if got := child.Get("write"); got != nil {
		t.Errorf("child should not have write")
	}
	if got := child.Get("nonexistent"); got != nil {
		t.Errorf("nonexistent should be silently skipped")
	}
}

func TestSpawn_PerCallModelRoutesToCorrectProvider(t *testing.T) {
	mockFast := llmtest.NewT(t)
	mockDeep := llmtest.NewT(t)
	pFast := &llm.Provider{Name: "fast", Client: mockFast, Model: "f", ContextWindow: 1_000_000, Pool: llm.NewPool(1)}
	pDeep := &llm.Provider{Name: "deep", Client: mockDeep, Model: "d", ContextWindow: 1_000_000, Pool: llm.NewPool(1)}

	mockDeep.Push(llmtest.Script{Text: "DEEP-ANSWER"})

	ac := &tools.AgentContext{
		Bus:          bus.New(),
		Permissions:  permissions.NewChecker(nil, nil, nil, "allow"),
		Provider:     pFast,
		Providers:    map[string]*llm.Provider{"fast": pFast, "deep": pDeep},
		Registry:     tools.NewRegistry(),
		Cwd:          t.TempDir(),
		MaxTurns:     5,
		Depth:        0,
		MaxDepth:     3,
		MaxAgents:    16,
		GlobalAgents: &atomic.Int64{},
		Transcripts:  tools.NewTranscripts(),
	}

	res, err := SpawnTool{}.Run(context.Background(),
		map[string]interface{}{"prompt": "deep think", "model": "deep"}, ac)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "DEEP-ANSWER") {
		t.Errorf("output = %q, want answer routed via deep", res.LLMOutput)
	}
	if mockFast.CallCount() != 0 {
		t.Errorf("fast should not have been called, got %d", mockFast.CallCount())
	}
	if mockDeep.CallCount() != 1 {
		t.Errorf("deep should have been called once, got %d", mockDeep.CallCount())
	}
}

func TestSpawn_UnknownModelArgReportsToLLM(t *testing.T) {
	mock := llmtest.NewT(t)
	p := &llm.Provider{Name: "fast", Client: mock, ContextWindow: 1_000_000, Pool: llm.NewPool(1)}
	ac := &tools.AgentContext{
		Bus:          bus.New(),
		Provider:     p,
		Providers:    map[string]*llm.Provider{"fast": p},
		Registry:     tools.NewRegistry(),
		Cwd:          t.TempDir(),
		MaxDepth:     3,
		MaxAgents:    16,
		GlobalAgents: &atomic.Int64{},
		Transcripts:  tools.NewTranscripts(),
	}
	res, err := SpawnTool{}.Run(context.Background(),
		map[string]interface{}{"prompt": "x", "model": "ghost"}, ac)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(res.LLMOutput, "unknown model") {
		t.Errorf("expected LLM-visible error, got %q", res.LLMOutput)
	}
	if mock.CallCount() != 0 {
		t.Errorf("validation should fail before any LLM call: %d", mock.CallCount())
	}
}
