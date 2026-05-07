// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/tools"
)

// fakeProvider builds a Provider whose Chat is `mock`. Pool concurrency
// controls whether sibling roles can actually run their Chat() calls in
// parallel — the runner uses the Pool to gate inflight LLM requests.
func fakeProvider(mock *llmtest.Mock, concurrency int) *llm.Provider {
	return &llm.Provider{
		Name:          "test",
		Client:        mock,
		Model:         "fake",
		ContextWindow: 1_000_000,
		Pool:          llm.NewPool(concurrency),
	}
}

func runDeps(t *testing.T, provider *llm.Provider, busInst *bus.Bus) RunDeps {
	return RunDeps{
		Providers:       map[string]*llm.Provider{provider.Name: provider},
		DefaultProvider: provider.Name,
		Bus:             busInst,
		Registry:        tools.NewRegistry(),
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		MaxTurns:        5,
		GlobalAgents:    &atomic.Int64{},
		MaxAgents:       16,
		MaxDepth:        3,
		Transcripts:     tools.NewTranscripts(),
	}
}

func TestRun_LinearPipelineFlowsOutputs(t *testing.T) {
	src := []byte(`---
roles:
  planner: {}
  coder: {}
  reviewer: {}
edges:
  - planner -> coder
  - coder -> reviewer
---

## planner

Plan: {{ .Args }}

## coder

{{ .planner.output }}

## reviewer

{{ .coder.output }}
`)
	wf, err := Parse("pipe.md", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "PLAN-OUT"})
	mock.Push(llmtest.Script{Text: "CODE-OUT"})
	mock.Push(llmtest.Script{Text: "REVIEW-OUT"})

	res, err := Run(context.Background(), wf, "user-args", runDeps(t, fakeProvider(mock, 1), bus.New()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outputs["planner"] != "PLAN-OUT" || res.Outputs["coder"] != "CODE-OUT" || res.Outputs["reviewer"] != "REVIEW-OUT" {
		t.Errorf("outputs: %+v", res.Outputs)
	}
	if res.Last != "REVIEW-OUT" {
		t.Errorf("Last = %q, want REVIEW-OUT", res.Last)
	}

	// The coder's prompt should have included the planner's output, and
	// the reviewer's the coder's. We verify by inspecting the requests
	// that hit the mock — request 2 (coder) must reference PLAN-OUT,
	// request 3 (reviewer) must reference CODE-OUT.
	calls := mock.Calls()
	if len(calls) != 3 {
		t.Fatalf("want 3 turns, got %d", len(calls))
	}
	mustContain := func(idx int, want string) {
		t.Helper()
		// The user prompt is always the last non-system message.
		var found bool
		for _, m := range calls[idx].Messages {
			if m.Role == "user" && containsStr(m.Content, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("turn %d prompt did not include %q: %+v", idx, want, calls[idx].Messages)
		}
	}
	mustContain(1, "PLAN-OUT")
	mustContain(2, "CODE-OUT")
}

func TestRun_SiblingsRunInParallel(t *testing.T) {
	// A and B are independent siblings; C depends on both. With Pool
	// concurrency=2, A and B's Chat calls should overlap in time. We
	// gate both, wait for the mock to record 2 in-flight calls, then
	// release them.
	src := []byte(`---
roles:
  alpha: {}
  beta: {}
  gamma: {}
edges:
  - alpha -> gamma
  - beta -> gamma
---

## alpha

A: {{ .Args }}

## beta

B: {{ .Args }}

## gamma

{{ .alpha.output }} + {{ .beta.output }}
`)
	wf, err := Parse("fan.md", src)
	if err != nil {
		t.Fatal(err)
	}

	mock := llmtest.NewT(t)
	gateA := make(chan struct{})
	gateB := make(chan struct{})
	// Whichever sibling races into Chat first gets the first script;
	// both are gated, both block. Order doesn't matter — we only care
	// that both are inflight before either completes.
	mock.Push(llmtest.Script{Text: "out-1", Gate: gateA})
	mock.Push(llmtest.Script{Text: "out-2", Gate: gateB})
	mock.Push(llmtest.Script{Text: "FINAL"})

	resCh := make(chan *Result, 1)
	errCh := make(chan error, 1)
	go func() {
		r, err := Run(context.Background(), wf, "x", runDeps(t, fakeProvider(mock, 2), bus.New()))
		if err != nil {
			errCh <- err
			return
		}
		resCh <- r
	}()

	// Wait until both siblings have called Chat. If the runner serialised
	// them, only one would ever be inflight at a time — gateA blocks the
	// first call and CallCount() would be stuck at 1.
	deadline := time.Now().Add(2 * time.Second)
	for mock.CallCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("siblings did not run in parallel: CallCount stuck at %d", mock.CallCount())
		}
		time.Sleep(2 * time.Millisecond)
	}

	close(gateA)
	close(gateB)

	select {
	case res := <-resCh:
		if res.Outputs["gamma"] != "FINAL" {
			t.Errorf("gamma output: %q", res.Outputs["gamma"])
		}
		if mock.CallCount() != 3 {
			t.Errorf("expected 3 turns total, got %d", mock.CallCount())
		}
	case err := <-errCh:
		t.Fatalf("workflow err: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("workflow did not complete after gates released")
	}
}

func TestRun_SingleRoleFailureSurfaces(t *testing.T) {
	src := []byte(`---
roles:
  alpha: {}
---

## alpha

{{ .Args }}
`)
	wf, err := Parse("fail.md", src)
	if err != nil {
		t.Fatal(err)
	}

	mock := llmtest.New()
	mock.Push(llmtest.Script{Err: errFake{}})

	deps := runDeps(t, fakeProvider(mock, 1), bus.New())
	_, err = Run(context.Background(), wf, "x", deps)
	if err == nil {
		t.Fatal("expected error from failed alpha to surface")
	}
	if mock.CallCount() != 1 {
		t.Errorf("expected exactly one model turn, got %d", mock.CallCount())
	}
}

func TestRun_FailedRoleDoesNotDeadlockDownstream(t *testing.T) {
	// alpha → beta. alpha fails before beta can launch; beta must NOT
	// be launched, and Run must return the alpha error rather than
	// hanging on a never-arriving beta done event. Regression test
	// for the launched-vs-total drain bug.
	src := []byte(`---
roles:
  alpha: {}
  beta: {}
edges:
  - alpha -> beta
---

## alpha

{{ .Args }}

## beta

{{ .alpha.output }}
`)
	wf, err := Parse("fail-chain.md", src)
	if err != nil {
		t.Fatal(err)
	}

	mock := llmtest.New()
	mock.Push(llmtest.Script{Err: errFake{}})

	deps := runDeps(t, fakeProvider(mock, 1), bus.New())

	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), wf, "x", deps)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error to surface")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run deadlocked when failed role had unreached dependents")
	}
	if mock.CallCount() != 1 {
		t.Errorf("beta should not have launched: CallCount=%d", mock.CallCount())
	}
}

func TestRun_FailedSiblingDoesNotBlockOtherSiblings(t *testing.T) {
	// Two independent siblings; one fails, the other should still
	// complete. Cancellation should propagate via context, but a
	// sibling already mid-Chat may still publish before the cancel
	// reaches it. Either way, Run must not hang.
	src := []byte(`---
roles:
  alpha: {}
  beta: {}
---

## alpha

{{ .Args }}

## beta

{{ .Args }}
`)
	wf, err := Parse("fail-fan.md", src)
	if err != nil {
		t.Fatal(err)
	}

	mock := llmtest.New()
	// Whichever lands first gets the error; the other gets the text.
	// Since both run in parallel and the queue is FIFO, either
	// ordering is valid; we only assert no hang and that the error
	// surfaces.
	mock.Push(llmtest.Script{Err: errFake{}})
	mock.Push(llmtest.Script{Text: "fine"})

	deps := runDeps(t, fakeProvider(mock, 2), bus.New())

	done := make(chan error, 1)
	go func() {
		_, err := Run(context.Background(), wf, "x", deps)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from failing sibling")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run deadlocked")
	}
}

func TestRun_PerRoleModelRoutesToCorrectProvider(t *testing.T) {
	src := []byte(`---
roles:
  alpha:
    model: deep
  beta: {}
edges:
  - alpha -> beta
---

## alpha

A: {{ .Args }}

## beta

{{ .alpha.output }}
`)
	wf, err := Parse("perrole.md", src)
	if err != nil {
		t.Fatal(err)
	}

	mockFast := llmtest.NewT(t)
	mockDeep := llmtest.NewT(t)

	pFast := &llm.Provider{Name: "fast", Client: mockFast, Model: "f", ContextWindow: 1_000_000, Pool: llm.NewPool(1)}
	pDeep := &llm.Provider{Name: "deep", Client: mockDeep, Model: "d", ContextWindow: 1_000_000, Pool: llm.NewPool(1)}

	mockDeep.Push(llmtest.Script{Text: "ALPHA-OUT"}) // alpha runs on deep
	mockFast.Push(llmtest.Script{Text: "BETA-OUT"})  // beta inherits default fast

	deps := RunDeps{
		Providers:       map[string]*llm.Provider{"fast": pFast, "deep": pDeep},
		DefaultProvider: "fast",
		Bus:             bus.New(),
		Registry:        tools.NewRegistry(),
		Perms:           permissions.NewChecker(nil, nil, nil, "allow"),
		Cwd:             t.TempDir(),
		MaxTurns:        5,
		GlobalAgents:    &atomic.Int64{},
		MaxAgents:       16,
		MaxDepth:        3,
		Transcripts:     tools.NewTranscripts(),
	}

	res, err := Run(context.Background(), wf, "x", deps)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outputs["alpha"] != "ALPHA-OUT" || res.Outputs["beta"] != "BETA-OUT" {
		t.Errorf("outputs: %+v", res.Outputs)
	}
	if mockDeep.CallCount() != 1 {
		t.Errorf("deep should have run once (alpha), got %d", mockDeep.CallCount())
	}
	if mockFast.CallCount() != 1 {
		t.Errorf("fast should have run once (beta), got %d", mockFast.CallCount())
	}
}

func TestRun_UnknownModelRejectedBeforeWork(t *testing.T) {
	src := []byte(`---
roles:
  alpha:
    model: doesnotexist
---

## alpha

{{ .Args }}
`)
	wf, err := Parse("badmodel.md", src)
	if err != nil {
		t.Fatal(err)
	}

	mock := llmtest.New()
	deps := runDeps(t, fakeProvider(mock, 1), bus.New())

	_, err = Run(context.Background(), wf, "x", deps)
	if err == nil {
		t.Fatal("expected error from unknown model name")
	}
	if mock.CallCount() != 0 {
		t.Errorf("model validation should have failed before any LLM call: %d", mock.CallCount())
	}
}

type errFake struct{}

func (errFake) Error() string { return "fake llm error" }

func containsStr(haystack, needle string) bool {
	if needle == "" {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
