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

// TestRun_ResetsTurnAllowsBetweenUserMessages locks in the load-bearing
// security property of P2 #13: a turn-scoped permission grant must
// expire when the next *real* user message arrives. Sub-agent fan-out
// inside one turn keeps the grant live; new user input clears it.
func TestRun_ResetsTurnAllowsBetweenUserMessages(t *testing.T) {
	mock := llmtest.NewT(t)
	// One trivial turn, no tool calls — we're verifying the reset
	// fires, not testing the chat path itself.
	mock.Push(llmtest.Script{Text: "first done"})

	checker := permissions.NewChecker(nil, nil, nil, "allow")
	if err := checker.AddTurnAllow("bash(go *)"); err != nil {
		t.Fatal(err)
	}
	if !checker.HasTurnAllows() {
		t.Fatal("precondition: turn grant should be active")
	}

	a, err := New(Config{
		Providers:       map[string]*llm.Provider{"test": fakeProvider(mock)},
		DefaultProvider: "test",
		Bus:             bus.New(),
		Registry:        tools.NewRegistry(),
		Perms:           checker,
		Cwd:             t.TempDir(),
		MaxTurns:        4,
	})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	inputCh := make(chan string, 2)
	runDone := make(chan struct{})
	go func() {
		_ = a.Run(ctx, inputCh)
		close(runDone)
	}()

	// First user message: agent's Run should call ResetTurnAllows
	// before processing.
	inputCh <- "hello"

	deadline := time.After(2 * time.Second)
	for checker.HasTurnAllows() {
		select {
		case <-deadline:
			t.Fatal("turn grant survived the user message")
		default:
			time.Sleep(2 * time.Millisecond)
		}
	}

	close(inputCh)
	cancel()
	<-runDone
}
