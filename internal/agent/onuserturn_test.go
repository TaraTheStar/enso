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

// seqWriter is a minimal tools.SessionWriter that hands out increasing
// seqs (mimicking session.Writer) so OnUserTurn receives a non-zero seq.
type seqWriter struct{ seq int }

func (w *seqWriter) AppendMessage(msg llm.Message, agentID string) (int, error) {
	w.seq++
	return w.seq, nil
}
func (w *seqWriter) AppendMessageUsage(seq int, usage llm.MessageUsage, agentID string) error {
	return nil
}
func (w *seqWriter) AppendToolCall(callID, name string, args map[string]any, llmOutput, fullOutput, status string) error {
	return nil
}
func (w *seqWriter) SessionID() string { return "test-sess" }

// TestRun_OnUserTurnFiresPerGenuineTurn confirms OnUserTurn fires once
// per real user submit, with that user message's seq, before inference —
// the per-turn checkpoint trigger for /rewind.
func TestRun_OnUserTurnFiresPerGenuineTurn(t *testing.T) {
	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "reply 1"})
	mock.Push(llmtest.Script{Text: "reply 2"})

	busInst := bus.New()
	gotSeqs := make(chan int, 8)

	a, err := New(Config{
		Providers:       map[string]*llm.Provider{"test": fakeProvider(mock)},
		DefaultProvider: "test",
		Bus:             busInst,
		Registry:        tools.NewRegistry(),
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		MaxTurns:        10,
		Writer:          &seqWriter{},
		OnUserTurn:      func(seq int) { gotSeqs <- seq },
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	events := busInst.Subscribe(64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputCh := make(chan UserInput, 8)
	go func() { _ = a.Run(ctx, inputCh) }()

	// Turn 1: user message lands at seq 1.
	inputCh <- UserInput{Text: "first"}
	if got := waitSeq(t, gotSeqs, "first turn callback"); got != 1 {
		t.Errorf("turn 1 OnUserTurn seq = %d, want 1", got)
	}
	waitFor(t, events, bus.EventAssistantDone, "turn 1 did not complete")

	// Turn 2: user=seq3 (turn1: user=1, assistant=2).
	inputCh <- UserInput{Text: "second"}
	if got := waitSeq(t, gotSeqs, "second turn callback"); got != 3 {
		t.Errorf("turn 2 OnUserTurn seq = %d, want 3", got)
	}

	close(inputCh)
	cancel()
}

// TestRunOneShot_DoesNotFireOnUserTurn confirms sub-agent RunOneShot
// turns (and, by the same path, recovery nudges) do NOT trigger a
// checkpoint — only genuine top-level user submits do.
func TestRunOneShot_DoesNotFireOnUserTurn(t *testing.T) {
	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "oneshot reply"})

	fired := false
	a, err := New(Config{
		Providers:       map[string]*llm.Provider{"test": fakeProvider(mock)},
		DefaultProvider: "test",
		Bus:             bus.New(),
		Registry:        tools.NewRegistry(),
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		MaxTurns:        10,
		Writer:          &seqWriter{},
		OnUserTurn:      func(seq int) { fired = true },
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	if _, err := a.RunOneShot(context.Background(), "do the thing"); err != nil {
		t.Fatalf("RunOneShot: %v", err)
	}
	if fired {
		t.Error("OnUserTurn fired for RunOneShot — only genuine Run turns should checkpoint")
	}
}

func waitSeq(t *testing.T, ch <-chan int, msg string) int {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout: %s", msg)
		return 0
	}
}
