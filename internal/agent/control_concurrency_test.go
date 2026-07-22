// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/provider"
	"github.com/TaraTheStar/enso/internal/tools"
)

// TestControlOps_NoRaceWithRunningTurn hammers the control-plane entry
// points (worker adapter RPCs / in-process slash commands run these off
// the agent goroutine) while a multi-tool-call turn is in flight. Before
// the history lock landed, this was a data race: ForcePrune rewrote
// History/toolMeta in place while the run loop appended, and
// PrefixBreakdown iterated toolMeta concurrently with its writes —
// a concurrent map read/write is a runtime fatal. Run with -race.
func TestControlOps_NoRaceWithRunningTurn(t *testing.T) {
	mock := llmtest.NewT(t)
	const toolTurns = 15
	for i := 0; i < toolTurns; i++ {
		mock.Push(llmtest.Script{ToolCalls: []llm.ToolCall{
			makeToolCall(fmt.Sprintf("c%d", i), "echo", `{"text":"x"}`),
		}})
	}
	mock.Push(llmtest.Script{Text: "done"})

	tool := &recordTool{}
	registry := tools.NewRegistry()
	registry.Register(tool)

	a, err := New(Config{
		Providers:       map[string]*provider.Provider{"test": fakeProvider(mock)},
		DefaultProvider: "test",
		Bus:             bus.New(),
		Registry:        registry,
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		MaxTurns:        toolTurns + 5,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				a.ForcePrune()
				_ = a.PrefixBreakdown()
				_ = a.CompactPreview()
				_ = a.EstimateTokens()
				_ = a.SetProvider("test")
				time.Sleep(100 * time.Microsecond)
			}
		}()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.RunOneShot(ctx, "go"); err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	close(stop)
	wg.Wait()

	if got := len(tool.calls); got != toolTurns {
		t.Errorf("tool ran %d times, want %d (control ops must not corrupt the turn loop)", got, toolTurns)
	}
}

// TestRequestPrompt_TimesOutToDeny: Bus.Publish is lossy — a permission
// request that nobody answers (dropped by a full subscriber buffer, or
// no subscriber at all) must degrade to deny after the timeout instead
// of hanging the agent goroutine forever.
func TestRequestPrompt_TimesOutToDeny(t *testing.T) {
	old := permissionPromptTimeout
	permissionPromptTimeout = 25 * time.Millisecond
	defer func() { permissionPromptTimeout = old }()

	a := &Agent{
		// No subscribers: the request is published into the void, which is
		// observationally identical to a dropped event — respCh never fires.
		Bus:      bus.New(),
		AgentCtx: &tools.AgentContext{Logger: slog.Default()},
	}

	done := make(chan permissions.Decision, 1)
	go func() {
		done <- a.requestPrompt(context.Background(), "bash", map[string]any{"command": "ls"})
	}()

	select {
	case d := <-done:
		if d != permissions.Deny {
			t.Errorf("decision = %v, want Deny", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("requestPrompt hung past the timeout — lost request not degraded to deny")
	}
}
