// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/tools"
)

// TestUsage_StampedAfterTurn confirms that an EventUsage emitted by the
// adapter lands on the agent's lastUsage / messageUsage state and feeds
// the cumulative counters.
func TestUsage_StampedAfterTurn(t *testing.T) {
	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{
		Text: "hi",
		Usage: &llm.MessageUsage{
			InputTokens:  120,
			OutputTokens: 30,
			TotalTokens:  150,
		},
	})

	a := newRunnableAgent(t, mock)
	driveOneTurn(t, a, "go")

	if got := a.LastUsage(); got.TotalTokens != 150 || got.InputTokens != 120 || got.OutputTokens != 30 {
		t.Errorf("lastUsage = %+v, want in=120 out=30 total=150", got)
	}
	if a.CumulativeInputTokens() != 120 {
		t.Errorf("cumIn = %d, want 120", a.CumulativeInputTokens())
	}
	if a.CumulativeOutputTokens() != 30 {
		t.Errorf("cumOut = %d, want 30", a.CumulativeOutputTokens())
	}
}

// TestUsage_CompactionUsesRealNumbers verifies that MaybeCompact reads
// from lastUsage, not the 4-char heuristic. With a deliberately small
// ContextWindow and a large reported input count, compaction MUST
// trigger even though llm.Estimate(History) would say otherwise.
func TestUsage_CompactionUsesRealNumbers(t *testing.T) {
	// Build a short history that would heuristic-estimate well under
	// any normal threshold.
	a := &Agent{
		History: []llm.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hi"},
			{Role: "assistant", Content: "ok"},
		},
		messageUsage: map[int]llm.MessageUsage{},
	}
	a.estTokens.Store(int64(llm.Estimate(a.History)))

	heuristic := llm.Estimate(a.History)
	if heuristic > 100 {
		t.Fatalf("test premise broken: heuristic=%d (expected tiny)", heuristic)
	}

	usage := llm.MessageUsage{InputTokens: 80_000, OutputTokens: 100, TotalTokens: 80_100}
	a.lastUsage.Store(&usage)
	a.messageUsage[len(a.History)-1] = usage

	// estimateTokens MUST prefer the real numbers.
	if est := a.estimateTokens(); est < 80_000 {
		t.Errorf("estimateTokens=%d, want >=80000 (should use real usage)", est)
	}
}

// TestUsage_FallsBackWhenAbsent confirms that with no recorded usage,
// estimateTokens uses the heuristic.
func TestUsage_FallsBackWhenAbsent(t *testing.T) {
	a := &Agent{
		History: []llm.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "hello world this is a longer message"},
			{Role: "assistant", Content: "reply"},
		},
		messageUsage: map[int]llm.MessageUsage{},
	}
	if a.lastUsage.Load() != nil {
		t.Fatal("test premise: lastUsage should start nil")
	}
	want := llm.Estimate(a.History)
	if got := a.estimateTokens(); got != want {
		t.Errorf("estimateTokens=%d, want %d (heuristic)", got, want)
	}
}

// TestUsage_ClearedOnCompaction confirms that lastUsage and the
// messageUsage map are reset after a compaction rebuilds History — the
// stamped InputTokens count described a History prefix that no longer
// exists, so it must not bleed into the next threshold check.
func TestUsage_ClearedOnCompaction(t *testing.T) {
	// Build a history big enough — in tokens, not just turn count — that
	// compactionBoundary returns ok under the token-budget tail (which
	// would otherwise happily pin all of a tiny test history). 12 turns
	// × ~4 KB each ≈ ~12 KB tokens, easily over the 2-8 KB trailing-turn
	// budget; oldest turns spill into the older block.
	bulk := strings.Repeat("data ", 800) // ~4 KB
	hist := []llm.Message{{Role: "system", Content: "sys"}}
	for i := 0; i < 12; i++ {
		hist = append(hist,
			llm.Message{Role: "user", Content: bulk},
			llm.Message{Role: "assistant", Content: bulk},
		)
	}

	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "structured summary placeholder"})

	a := &Agent{
		History:         hist,
		Bus:             bus.New(),
		Providers:       map[string]*llm.Provider{"test": fakeProvider(mock)},
		currentProvider: fakeProvider(mock),
		messageUsage:    map[int]llm.MessageUsage{},
	}
	a.AgentCtx = &tools.AgentContext{Provider: a.currentProvider}

	// Plant a usage record so we can verify it's cleared.
	usage := llm.MessageUsage{InputTokens: 50_000, OutputTokens: 200, TotalTokens: 50_200}
	a.lastUsage.Store(&usage)
	a.messageUsage[len(a.History)-1] = usage

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := a.forceCompact(ctx, "test"); err != nil {
		t.Fatalf("forceCompact: %v", err)
	}

	if u := a.lastUsage.Load(); u != nil {
		t.Errorf("lastUsage = %+v, want nil after compaction", *u)
	}
	a.messageUsageMu.Lock()
	n := len(a.messageUsage)
	a.messageUsageMu.Unlock()
	if n != 0 {
		t.Errorf("messageUsage size = %d, want 0 after compaction", n)
	}
}

// helpers

func newRunnableAgent(t *testing.T, mock *llmtest.Mock) *Agent {
	t.Helper()
	a, err := New(Config{
		Providers:       map[string]*llm.Provider{"test": fakeProvider(mock)},
		DefaultProvider: "test",
		Bus:             bus.New(),
		Registry:        tools.NewRegistry(),
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		MaxTurns:        4,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}

func driveOneTurn(t *testing.T, a *Agent, prompt string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputCh := make(chan string, 1)
	done := make(chan struct{})
	go func() { _ = a.Run(ctx, inputCh); close(done) }()
	inputCh <- prompt

	deadline := time.After(2 * time.Second)
	for a.CumulativeOutputTokens() == 0 {
		select {
		case <-deadline:
			t.Fatalf("turn never completed")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}
	close(inputCh)
	cancel()
	<-done
}
