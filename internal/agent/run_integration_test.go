// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/azoth/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/provider"
	"github.com/TaraTheStar/enso/internal/tools"
)

// recordTool is a minimal Tool used by these integration tests. It
// echoes its `text` arg into the LLM-bound result and records every
// invocation so the test can assert tool dispatch.
type recordTool struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (t *recordTool) Name() string        { return "echo" }
func (t *recordTool) Description() string { return "echo the given text" }
func (t *recordTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string"},
		},
		"required": []string{"text"},
	}
}
func (t *recordTool) Run(ctx context.Context, args map[string]any, ac *tools.AgentContext) (tools.Result, error) {
	t.mu.Lock()
	t.calls = append(t.calls, args)
	t.mu.Unlock()
	text, _ := args["text"].(string)
	return tools.Result{LLMOutput: "echoed: " + text, FullOutput: "echoed: " + text}, nil
}

// fakeProvider builds an provider.Provider whose Chat is the given mock.
// ContextWindow is set high enough that compaction never triggers in
// tests that don't explicitly want it.
func fakeProvider(mock *llmtest.Mock) *provider.Provider {
	return &provider.Provider{
		Name:          "test",
		Client:        mock,
		Model:         "fake",
		ContextWindow: 1_000_000,
		Pool:          llm.NewPool(1),
	}
}

// makeToolCall builds an llm.ToolCall with the anonymous Function
// struct populated.
func makeToolCall(id, name, args string) llm.ToolCall {
	tc := llm.ToolCall{ID: id, Type: "function"}
	tc.Function.Name = name
	tc.Function.Arguments = args
	return tc
}

func TestRunOneShot_ToolCallInterleavedWithFinalAnswer(t *testing.T) {
	mock := llmtest.NewT(t)

	// Turn 1: model asks for echo("hi")
	mock.Push(llmtest.Script{ToolCalls: []llm.ToolCall{makeToolCall("c1", "echo", `{"text":"hi"}`)}})
	// Turn 2: model produces the final reply
	mock.Push(llmtest.Script{Text: "tool said: echoed: hi"})

	tool := &recordTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	checker := permissions.NewChecker(nil, nil, nil, "allow") // auto-allow

	a, err := New(Config{
		Providers:       map[string]*provider.Provider{"test": fakeProvider(mock)},
		DefaultProvider: "test",
		Bus:             bus.New(),
		Registry:        registry,
		Perms:           checker,
		Cwd:             t.TempDir(),
		MaxTurns:        10,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	final, err := a.RunOneShot(context.Background(), "please echo hi")
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}

	if !strings.Contains(final, "echoed: hi") {
		t.Errorf("final answer missing tool output: %q", final)
	}
	if mock.CallCount() != 2 {
		t.Errorf("want 2 model turns, got %d", mock.CallCount())
	}
	if got := len(tool.calls); got != 1 {
		t.Fatalf("want 1 tool call, got %d", got)
	}
	if tool.calls[0]["text"] != "hi" {
		t.Errorf("tool args lost: %+v", tool.calls[0])
	}

	// The second model turn must have seen the tool result in its
	// message history; otherwise the agent isn't feeding tool replies
	// back correctly.
	calls := mock.Calls()
	turn2 := calls[1].Messages
	var sawToolReply bool
	for _, m := range turn2 {
		if m.Role == "tool" && strings.Contains(m.Content, "echoed: hi") {
			sawToolReply = true
			break
		}
	}
	if !sawToolReply {
		t.Errorf("turn 2 messages missing tool reply: %+v", turn2)
	}
}

func TestRun_MultipleConsecutiveUserMessages(t *testing.T) {
	mock := llmtest.NewT(t)

	// User msg 1: tool call → final answer
	mock.Push(llmtest.Script{ToolCalls: []llm.ToolCall{makeToolCall("c1", "echo", `{"text":"one"}`)}})
	mock.Push(llmtest.Script{Text: "first done"})
	// User msg 2: direct final answer (no tool)
	mock.Push(llmtest.Script{Text: "second done"})

	tool := &recordTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	busInst := bus.New()
	checker := permissions.NewChecker(nil, nil, nil, "allow")

	a, err := New(Config{
		Providers:       map[string]*provider.Provider{"test": fakeProvider(mock)},
		DefaultProvider: "test",
		Bus:             busInst,
		Registry:        registry,
		Perms:           checker,
		Cwd:             t.TempDir(),
		MaxTurns:        10,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	// We feed prompts one at a time, waiting for AssistantDone before
	// sending the next so the test exercises the input-cycle, not just
	// queueing.
	doneCh := busInst.Subscribe(8)
	waitForDone := func() {
		for {
			select {
			case ev := <-doneCh:
				if ev.Type == bus.EventAssistantDone {
					return
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("timed out waiting for AssistantDone")
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputCh := make(chan UserInput, 1)

	runErr := make(chan error, 1)
	go func() { runErr <- a.Run(ctx, inputCh) }()

	inputCh <- UserInput{Text: "first prompt"}
	waitForDone()
	inputCh <- UserInput{Text: "second prompt"}
	waitForDone()

	close(inputCh)
	if err := <-runErr; err != nil {
		t.Errorf("Run returned: %v", err)
	}

	if mock.CallCount() != 3 {
		t.Errorf("expected 3 model turns (2 for first prompt + 1 for second), got %d", mock.CallCount())
	}
	if got := len(tool.calls); got != 1 {
		t.Errorf("expected 1 tool call across both prompts, got %d", got)
	}
}

func TestSetProvider_SwitchesActiveProvider(t *testing.T) {
	mockA := llmtest.New()
	mockB := llmtest.New()

	pA := &provider.Provider{Name: "fast", Client: mockA, Model: "f", ContextWindow: 1_000_000, Pool: llm.NewPool(1)}
	pB := &provider.Provider{Name: "deep", Client: mockB, Model: "d", ContextWindow: 1_000_000, Pool: llm.NewPool(1)}

	a, err := New(Config{
		Providers:       map[string]*provider.Provider{"fast": pA, "deep": pB},
		DefaultProvider: "fast",
		Bus:             bus.New(),
		Registry:        tools.NewRegistry(),
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		MaxTurns:        5,
	})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	if got := a.ProviderName(); got != "fast" {
		t.Errorf("initial provider = %q, want fast", got)
	}
	if err := a.SetProvider("deep"); err != nil {
		t.Fatalf("set deep: %v", err)
	}
	if got := a.ProviderName(); got != "deep" {
		t.Errorf("after switch: %q, want deep", got)
	}
	if err := a.SetProvider("nope"); err == nil {
		t.Errorf("expected error on unknown name")
	}
	// Unknown name doesn't change current provider.
	if got := a.ProviderName(); got != "deep" {
		t.Errorf("after failed switch: %q, want deep (unchanged)", got)
	}
}

func TestNew_DefaultProviderFallsBackToAlphabeticalFirst(t *testing.T) {
	pA := &provider.Provider{Name: "alpha", Client: llmtest.New(), ContextWindow: 1_000, Pool: llm.NewPool(1)}
	pB := &provider.Provider{Name: "beta", Client: llmtest.New(), ContextWindow: 1_000, Pool: llm.NewPool(1)}

	a, err := New(Config{
		Providers: map[string]*provider.Provider{"beta": pB, "alpha": pA},
		// DefaultProvider intentionally empty
		Bus:      bus.New(),
		Registry: tools.NewRegistry(),
		Perms:    permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:      t.TempDir(),
		MaxTurns: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := a.ProviderName(); got != "alpha" {
		t.Errorf("alphabetical-first = %q, want alpha", got)
	}
}

func TestNew_RejectsUnknownDefaultProvider(t *testing.T) {
	pA := &provider.Provider{Name: "alpha", Client: llmtest.New(), ContextWindow: 1_000, Pool: llm.NewPool(1)}
	_, err := New(Config{
		Providers:       map[string]*provider.Provider{"alpha": pA},
		DefaultProvider: "missing",
		Bus:             bus.New(),
		Registry:        tools.NewRegistry(),
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		MaxTurns:        5,
	})
	if err == nil {
		t.Fatal("expected error on unknown default_provider")
	}
}

func TestNew_RejectsEmptyProviders(t *testing.T) {
	_, err := New(Config{
		Bus:      bus.New(),
		Registry: tools.NewRegistry(),
		Perms:    permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:      t.TempDir(),
		MaxTurns: 5,
	})
	if err == nil {
		t.Fatal("expected error on empty Providers")
	}
}

func TestRunOneShot_DenyPatternBlocksTool(t *testing.T) {
	mock := llmtest.NewT(t)

	// Turn 1: model asks for echo, but it's denied
	mock.Push(llmtest.Script{ToolCalls: []llm.ToolCall{makeToolCall("c1", "echo", `{"text":"hi"}`)}})
	// Turn 2: model gives up after seeing the denied tool result
	mock.Push(llmtest.Script{Text: "got denied, giving up"})

	tool := &recordTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	// Deny every echo call.
	checker := permissions.NewChecker(nil, nil, []string{"echo(*)"}, "allow")

	a, err := New(Config{
		Providers:       map[string]*provider.Provider{"test": fakeProvider(mock)},
		DefaultProvider: "test",
		Bus:             bus.New(),
		Registry:        registry,
		Perms:           checker,
		Cwd:             t.TempDir(),
		MaxTurns:        10,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	final, err := a.RunOneShot(context.Background(), "please echo hi")
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if !strings.Contains(final, "giving up") {
		t.Errorf("expected give-up answer, got %q", final)
	}
	if got := len(tool.calls); got != 0 {
		t.Errorf("denied tool must not run, but got %d calls", got)
	}
}

// TestRunOneShot_InlineToolCallLeakedIntoReasoning is a regression test
// for Qwen3 thinking-style chat templates on llama.cpp that emit the
// tool call as pseudo-XML inside the reasoning_content channel instead
// of via the structured tool_calls channel or even the content channel.
// Before the second fallback in turn(), this surfaced as "model produced
// no visible response" because the inline parser only ran over content.
// The recovered call must execute and the turn must complete normally.
func TestRunOneShot_InlineToolCallLeakedIntoReasoning(t *testing.T) {
	mock := llmtest.NewT(t)

	// Turn 1: no Text, no structured ToolCalls — the call only exists
	// as inline pseudo-XML buried in the reasoning stream.
	mock.Push(llmtest.Script{Reasoning: "Let me echo that.\n" +
		"<tool_call>\n" +
		"<function=echo>\n" +
		"<parameter=text>hi from reasoning</parameter>\n" +
		"</function>\n" +
		"</tool_call>\n"})
	// Turn 2: model produces the final reply.
	mock.Push(llmtest.Script{Text: "tool said: echoed: hi from reasoning"})

	tool := &recordTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	checker := permissions.NewChecker(nil, nil, nil, "allow")

	a, err := New(Config{
		Providers:       map[string]*provider.Provider{"test": fakeProvider(mock)},
		DefaultProvider: "test",
		Bus:             bus.New(),
		Registry:        registry,
		Perms:           checker,
		Cwd:             t.TempDir(),
		MaxTurns:        10,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	final, err := a.RunOneShot(context.Background(), "please echo hi")
	if err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}

	if got := len(tool.calls); got != 1 {
		t.Fatalf("inline call leaked into reasoning was not recovered: want 1 tool call, got %d", got)
	}
	if tool.calls[0]["text"] != "hi from reasoning" {
		t.Errorf("tool args lost in reasoning recovery: %+v", tool.calls[0])
	}
	if !strings.Contains(final, "echoed: hi from reasoning") {
		t.Errorf("final answer missing tool output: %q", final)
	}
	if mock.CallCount() != 2 {
		t.Errorf("want 2 model turns, got %d", mock.CallCount())
	}
}
