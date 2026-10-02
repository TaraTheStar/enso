// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/azoth/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/provider"
	"github.com/TaraTheStar/enso/internal/tools"
)

// recoverProvider is fakeProvider with the auto-recovery policy enabled,
// mirroring what provider.go wires for the OpenAI/llama.cpp path.
func recoverProvider(mock *llmtest.Mock, attempts int) *provider.Provider {
	p := fakeProvider(mock)
	p.AutoRecover = true
	p.MaxRecoverAttempts = attempts
	return p
}

func newRecoverAgent(t *testing.T, p *provider.Provider) (*Agent, *bus.Bus) {
	t.Helper()
	b := bus.New()
	a, err := New(Config{
		Providers:       map[string]*provider.Provider{"test": p},
		DefaultProvider: "test",
		Bus:             b,
		Registry:        tools.NewRegistry(),
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		MaxTurns:        20,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a, b
}

// driveOnce feeds a single prompt and waits until the agent loop goes
// idle (the whole user-message → reply pipeline, including recovery
// retries and tool rounds, is done).
func driveOnce(t *testing.T, a *Agent, b *bus.Bus, prompt string) []bus.Event {
	t.Helper()
	sub := b.Subscribe(64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputCh := make(chan UserInput, 1)
	runDone := make(chan struct{})
	go func() { _ = a.Run(ctx, inputCh); close(runDone) }()

	inputCh <- UserInput{Text: prompt}

	var events []bus.Event
	for {
		select {
		case ev := <-sub:
			events = append(events, ev)
			if ev.Type == bus.EventAgentIdle {
				close(inputCh)
				cancel()
				<-runDone
				return events
			}
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for EventAgentIdle")
		}
	}
}

// TestRecover_RepetitionRetriesWithNudge: a turn that trips the loop
// guard is discarded and retried; the second turn succeeds. The
// degenerate partial must NOT appear in history, and a recovery nudge
// must have been injected.
func TestRecover_RepetitionRetriesWithNudge(t *testing.T) {
	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "LOOP LOOP LOOP LOOP", FinishReason: llm.FinishRepetition})
	mock.Push(llmtest.Script{Text: "Here is the real answer."})

	a, b := newRecoverAgent(t, recoverProvider(mock, 2))
	driveOnce(t, a, b, "do the thing")

	if mock.CallCount() != 2 {
		t.Fatalf("expected 2 model calls (1 degenerate + 1 retry), got %d", mock.CallCount())
	}

	// The degenerate text must not have been persisted.
	for _, m := range a.History {
		if m.Role == "assistant" && strings.Contains(m.Content, "LOOP") {
			t.Errorf("degenerate partial leaked into history: %q", m.Content)
		}
	}
	// A user-role recovery nudge must have been injected between turns.
	var nudged bool
	for _, m := range a.History {
		if m.Role == "user" && strings.Contains(strings.ToLower(m.Content), "repeating") {
			nudged = true
		}
	}
	if !nudged {
		t.Error("expected a recovery nudge injected into history")
	}
	// Final answer should be present.
	last := a.History[len(a.History)-1]
	if last.Role != "assistant" || !strings.Contains(last.Content, "real answer") {
		t.Errorf("final history entry = %+v, want the real answer", last)
	}
}

// TestRecover_GivesUpAfterMaxAttempts: a model that keeps repeating is
// stopped after MaxRecoverAttempts with a surfaced EventError, not an
// infinite loop.
func TestRecover_GivesUpAfterMaxAttempts(t *testing.T) {
	mock := llmtest.New() // not NewT: we intentionally leave no leftover assertion
	// 1 initial + 2 retries = 3 degenerate turns, then give up.
	for i := 0; i < 3; i++ {
		mock.Push(llmtest.Script{Text: "LOOP", FinishReason: llm.FinishRepetition})
	}

	a, b := newRecoverAgent(t, recoverProvider(mock, 2))
	events := driveOnce(t, a, b, "do the thing")

	if mock.CallCount() != 3 {
		t.Fatalf("expected 3 model calls (initial + 2 retries), got %d", mock.CallCount())
	}
	var sawErr bool
	for _, ev := range events {
		if ev.Type == bus.EventError {
			sawErr = true
		}
	}
	if !sawErr {
		t.Error("expected an EventError after exhausting recovery attempts")
	}
}

// TestRecover_LengthTruncationContinues: a clean length-truncation keeps
// the partial (it's real work) and continues with a follow-up turn.
func TestRecover_LengthTruncationContinues(t *testing.T) {
	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "first half of a long answer", FinishReason: llm.FinishLength})
	mock.Push(llmtest.Script{Text: " and the second half."})

	a, b := newRecoverAgent(t, recoverProvider(mock, 2))
	driveOnce(t, a, b, "write something long")

	if mock.CallCount() != 2 {
		t.Fatalf("expected 2 model calls (truncated + continuation), got %d", mock.CallCount())
	}
	// The truncated partial is real work and MUST be kept.
	var keptPartial bool
	for _, m := range a.History {
		if m.Role == "assistant" && strings.Contains(m.Content, "first half") {
			keptPartial = true
		}
	}
	if !keptPartial {
		t.Error("truncated partial should be kept in history, not discarded")
	}
	// A continue-nudge should have been injected.
	var continued bool
	for _, m := range a.History {
		if m.Role == "user" && strings.Contains(strings.ToLower(m.Content), "cut off") {
			continued = true
		}
	}
	if !continued {
		t.Error("expected a continue nudge after length truncation")
	}
}

// TestRecover_DisabledSurfacesNormally: with AutoRecover off, a
// repetition finish is not retried — the turn ends as-is.
func TestRecover_DisabledSurfacesNormally(t *testing.T) {
	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "LOOP LOOP", FinishReason: llm.FinishRepetition})

	p := fakeProvider(mock)
	p.AutoRecover = false
	a, b := newRecoverAgent(t, p)
	driveOnce(t, a, b, "do the thing")

	if mock.CallCount() != 1 {
		t.Fatalf("expected exactly 1 model call (no recovery), got %d", mock.CallCount())
	}
}

// TestRecover_EmptyTurnRetriesWithNudge: a turn that ends with reasoning
// only — whether the model stopped or ran out of output budget — is
// retried rather than surfaced as a dead turn.
func TestRecover_EmptyTurnRetriesWithNudge(t *testing.T) {
	for _, finish := range []string{"stop", llm.FinishLength} {
		t.Run(finish, func(t *testing.T) {
			mock := llmtest.NewT(t)
			mock.Push(llmtest.Script{Reasoning: "hmm, let me think", FinishReason: finish})
			mock.Push(llmtest.Script{Text: "the answer"})

			a, b := newRecoverAgent(t, recoverProvider(mock, 2))
			events := driveOnce(t, a, b, "do the thing")

			if mock.CallCount() != 2 {
				t.Fatalf("expected 2 model calls (empty + retry), got %d", mock.CallCount())
			}
			for _, ev := range events {
				if ev.Type == bus.EventError {
					t.Errorf("recovered turn must not surface an error, got %v", ev.Payload)
				}
			}
			var nudged bool
			for _, m := range a.History {
				if m.Role == "assistant" && m.Content == "" {
					t.Error("empty assistant message must not be persisted")
				}
				if m.Role == "user" && strings.Contains(m.Content, "without an answer") {
					nudged = true
				}
			}
			if !nudged {
				t.Error("expected an empty-turn nudge in history")
			}
		})
	}
}

// TestRecover_EmptyTurnExhaustedExplainsItself: once retries run out, the
// surfaced error names what actually happened.
func TestRecover_EmptyTurnExhaustedExplainsItself(t *testing.T) {
	mock := llmtest.NewT(t)
	for range 2 {
		mock.Push(llmtest.Script{Reasoning: "still thinking", FinishReason: llm.FinishLength})
	}

	a, b := newRecoverAgent(t, recoverProvider(mock, 1))
	events := driveOnce(t, a, b, "do the thing")

	if mock.CallCount() != 2 {
		t.Fatalf("expected 2 model calls (1 + 1 retry), got %d", mock.CallCount())
	}
	var got string
	for _, ev := range events {
		if ev.Type == bus.EventError {
			got = ev.Payload.(error).Error()
		}
	}
	if !strings.Contains(got, "output limit") {
		t.Errorf("expected an output-limit explanation, got %q", got)
	}
}

func TestEmptyTurnError(t *testing.T) {
	cases := []struct {
		name, finish, reasoning, want string
	}{
		{"length", llm.FinishLength, "thinking", "output limit"},
		{"unparsed tool call", "stop", "ok\n<tool_call>{broken", "could not be parsed"},
		{"reasoning only", "stop", "thinking", "finished thinking"},
		{"nothing", "stop", "", "empty response"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := emptyTurnError(c.finish, c.reasoning).Error(); !strings.Contains(got, c.want) {
				t.Errorf("got %q, want it to contain %q", got, c.want)
			}
		})
	}
}
