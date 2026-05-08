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

func TestRun_DrainsQueuedSubmitsOnCancel(t *testing.T) {
	// Scenario: user sends a prompt that starts an LLM turn; the turn
	// blocks (stream Gate). User queues two more submits. User cancels.
	// The drain must consume the two queued messages AND publish
	// EventInputDiscarded with count=2 — without the drain they'd
	// land as the next turn out of order.

	mock := llmtest.NewT(t)
	gate := make(chan struct{})
	mock.Push(llmtest.Script{Text: "hung", Gate: gate})

	registry := tools.NewRegistry()
	checker := permissions.NewChecker(nil, nil, nil, "allow")
	busInst := bus.New()

	a, err := New(Config{
		Providers:       map[string]*llm.Provider{"test": fakeProvider(mock)},
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

	events := busInst.Subscribe(64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputCh := make(chan string, 8)
	runDone := make(chan struct{})
	go func() {
		_ = a.Run(ctx, inputCh)
		close(runDone)
	}()

	// First message starts the turn. Wait for the user-message event so
	// we know the agent has consumed it and entered runUntilQuiescent.
	inputCh <- "first"
	waitFor(t, events, bus.EventUserMessage, "agent did not consume first prompt")

	// Queue two more while the turn is blocked on the LLM Gate.
	inputCh <- "queued-1"
	inputCh <- "queued-2"

	// Cancel the in-flight turn. The Gate goroutine is still blocked
	// (intentionally — simulates a stuck network read); release it so
	// the turn can finish unwinding.
	a.Cancel()
	close(gate)

	got := waitFor(t, events, bus.EventInputDiscarded, "drain did not publish discard event")
	count, ok := got.Payload.(int)
	if !ok || count != 2 {
		t.Errorf("EventInputDiscarded payload=%v want int(2)", got.Payload)
	}

	// Both queued messages must be gone — submitting close+drain on
	// the channel proves no leftover.
	close(inputCh)
	cancel()
	<-runDone
}

func TestRun_NoDrainOnNaturalCompletion(t *testing.T) {
	// A turn that completes without cancel must NOT drain queued
	// followups. This is the regression-guard against an over-eager
	// drain — a normal multi-message session would lose all but the
	// first message if we triggered drain on every turn boundary.
	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "first done"})
	mock.Push(llmtest.Script{Text: "second done"})

	registry := tools.NewRegistry()
	checker := permissions.NewChecker(nil, nil, nil, "allow")
	busInst := bus.New()

	a, err := New(Config{
		Providers:       map[string]*llm.Provider{"test": fakeProvider(mock)},
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

	events := busInst.Subscribe(64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputCh := make(chan string, 4)
	runDone := make(chan struct{})
	go func() {
		_ = a.Run(ctx, inputCh)
		close(runDone)
	}()

	// Submit both up-front; the second sits in the buffer while turn 1
	// runs to completion. A buggy drain would consume "second" instead
	// of letting turn 2 process it.
	inputCh <- "first"
	inputCh <- "second"

	// Wait for both turns to actually fire. If drain mistakenly ate
	// "second" we'd time out here.
	waitForN(t, events, bus.EventAgentIdle, 2, "expected two natural-completion turns")

	// And no discard event should have been emitted.
	deadline := time.After(50 * time.Millisecond)
	for {
		select {
		case ev := <-events:
			if ev.Type == bus.EventInputDiscarded {
				t.Errorf("unexpected EventInputDiscarded on natural completion: %v", ev.Payload)
				return
			}
		case <-deadline:
			close(inputCh)
			cancel()
			<-runDone
			return
		}
	}
}

func TestDrainInputCh_EmptyChannelReturnsZero(t *testing.T) {
	ch := make(chan string, 4)
	if n := drainInputCh(ch); n != 0 {
		t.Errorf("drainInputCh on empty=%d, want 0", n)
	}
}

func TestDrainInputCh_DrainsAll(t *testing.T) {
	ch := make(chan string, 4)
	ch <- "a"
	ch <- "b"
	ch <- "c"
	if n := drainInputCh(ch); n != 3 {
		t.Errorf("drainInputCh=%d, want 3", n)
	}
	// Channel must be empty after drain.
	select {
	case x := <-ch:
		t.Errorf("expected empty channel, got %q", x)
	default:
	}
}

// waitFor blocks until an event of `want` type is observed and returns
// it. Fails the test on timeout.
func waitFor(t *testing.T, ch <-chan bus.Event, want bus.EventType, msg string) bus.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("%s (timeout waiting for %v)", msg, want)
		}
	}
}

// waitForN waits for `n` events of `want` type. Fails on timeout.
func waitForN(t *testing.T, ch <-chan bus.Event, want bus.EventType, n int, msg string) {
	t.Helper()
	got := 0
	deadline := time.After(2 * time.Second)
	for got < n {
		select {
		case ev := <-ch:
			if ev.Type == want {
				got++
			}
		case <-deadline:
			t.Fatalf("%s (got %d/%d)", msg, got, n)
		}
	}
}
