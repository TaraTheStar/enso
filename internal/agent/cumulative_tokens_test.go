// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/tools"
)

func TestRun_AccumulatesCumulativeTokens(t *testing.T) {
	mock := llmtest.NewT(t)
	// Single turn, multi-word reply so the heuristic produces a
	// non-trivial output count.
	mock.Push(llmtest.Script{Text: "this is a moderately long assistant reply for token counting purposes"})

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

	if a.CumulativeInputTokens() != 0 {
		t.Errorf("cumIn=%d at startup, want 0", a.CumulativeInputTokens())
	}
	if a.CumulativeOutputTokens() != 0 {
		t.Errorf("cumOut=%d at startup, want 0", a.CumulativeOutputTokens())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputCh := make(chan string, 1)
	runDone := make(chan struct{})
	go func() {
		_ = a.Run(ctx, inputCh)
		close(runDone)
	}()

	inputCh <- "hello agent please respond"

	// Wait for the turn to complete (EventAgentIdle) then snapshot.
	deadline := time.After(2 * time.Second)
	for a.CumulativeOutputTokens() == 0 {
		select {
		case <-deadline:
			t.Fatalf("output never accumulated: in=%d out=%d", a.CumulativeInputTokens(), a.CumulativeOutputTokens())
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}

	if a.CumulativeInputTokens() <= 0 {
		t.Errorf("cumIn=%d after turn, want >0", a.CumulativeInputTokens())
	}
	if a.CumulativeOutputTokens() <= 0 {
		t.Errorf("cumOut=%d after turn, want >0", a.CumulativeOutputTokens())
	}

	close(inputCh)
	cancel()
	<-runDone
}

func TestRun_CumulativeSurvivesCompactionShrinkingHistory(t *testing.T) {
	// estTokens shrinks after compaction, but cumulative spend is
	// "what was paid for", which never decreases. Lock that in: a
	// fresh agent's cumulative reflects total spend regardless of
	// what's currently in history.
	a := &Agent{}
	a.cumIn.Add(50_000)
	a.cumOut.Add(20_000)
	a.estTokens.Store(15_000) // pretend compaction shrank context

	if a.CumulativeInputTokens() != 50_000 {
		t.Errorf("cumIn=%d, want 50000 (unaffected by estTokens)", a.CumulativeInputTokens())
	}
	if a.CumulativeOutputTokens() != 20_000 {
		t.Errorf("cumOut=%d, want 20000", a.CumulativeOutputTokens())
	}
	if a.EstimateTokens() != 15_000 {
		t.Errorf("estTokens=%d, want 15000 (unchanged)", a.EstimateTokens())
	}
}
