// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import (
	"context"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
)

// gatedShipSrc is a ship-vs-escalate router: review runs once and exactly
// one of ship/escalate fires based on review's structured verdict field.
const gatedShipSrc = `---
roles:
  review: {}
  ship: {}
  escalate: {}
edges:
  - review -> ship      if '{{ eq .review.verdict "LGTM" }}'
  - review -> escalate  if '{{ ne .review.verdict "LGTM" }}'
---

## review

Review: {{ .Args }}

## ship

Ship it.

## escalate

Escalate it.
`

// TestRun_GatedSkip_RejectRoutesToEscalate: a non-LGTM verdict skips ship and
// fires escalate. The skipped branch produces no LLM call and is flagged in
// Result.Skipped.
func TestRun_GatedSkip_RejectRoutesToEscalate(t *testing.T) {
	wf, err := Parse("gated.md", []byte(gatedShipSrc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "looks bad\n```json\n{\"verdict\":\"reject\"}\n```"})
	mock.Push(llmtest.Script{Text: "ESCALATED"})

	res, err := Run(context.Background(), wf, "x", runDeps(t, fakeProvider(mock, 1), bus.New()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Skipped["ship"] {
		t.Errorf("ship should be skipped, Skipped=%+v", res.Skipped)
	}
	if res.Skipped["escalate"] {
		t.Errorf("escalate should have run, Skipped=%+v", res.Skipped)
	}
	if res.Outputs["escalate"] != "ESCALATED" {
		t.Errorf("escalate output = %q", res.Outputs["escalate"])
	}
	if res.Outputs["ship"] != "" {
		t.Errorf("skipped ship should have empty output, got %q", res.Outputs["ship"])
	}
	if mock.CallCount() != 2 {
		t.Errorf("want 2 LLM calls (review + escalate), got %d — skip must not spawn a role", mock.CallCount())
	}
	// Last = last non-skipped role in topo order; ship is skipped so it
	// must not become the reported result.
	if res.Last != "ESCALATED" {
		t.Errorf("Last = %q, want ESCALATED", res.Last)
	}
}

// TestRun_GatedSkip_LGTMRoutesToShip: the LGTM path takes the ship branch and
// skips escalate — the mirror image of the reject case.
func TestRun_GatedSkip_LGTMRoutesToShip(t *testing.T) {
	wf, err := Parse("gated.md", []byte(gatedShipSrc))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "all good\n```json\n{\"verdict\":\"LGTM\"}\n```"})
	mock.Push(llmtest.Script{Text: "SHIPPED"})

	res, err := Run(context.Background(), wf, "x", runDeps(t, fakeProvider(mock, 1), bus.New()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Skipped["escalate"] {
		t.Errorf("escalate should be skipped, Skipped=%+v", res.Skipped)
	}
	if res.Outputs["ship"] != "SHIPPED" {
		t.Errorf("ship output = %q", res.Outputs["ship"])
	}
	if mock.CallCount() != 2 {
		t.Errorf("want 2 LLM calls (review + ship), got %d", mock.CallCount())
	}
}

// TestRun_TransitiveSkip: a false guard skips b, and the skip propagates down
// the unconditional b -> c edge so c never runs either. Only the root a runs.
func TestRun_TransitiveSkip(t *testing.T) {
	src := []byte(`---
roles:
  a: {}
  b: {}
  c: {}
edges:
  - a -> b if '{{ eq .a.verdict "go" }}'
  - b -> c
---

## a

A: {{ .Args }}

## b

{{ .a.output }}

## c

{{ .b.output }}
`)
	wf, err := Parse("transitive.md", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "stopping\n```json\n{\"verdict\":\"stop\"}\n```"})

	res, err := Run(context.Background(), wf, "x", runDeps(t, fakeProvider(mock, 1), bus.New()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Skipped["b"] || !res.Skipped["c"] {
		t.Errorf("b and c should both be skipped, Skipped=%+v", res.Skipped)
	}
	if res.Skipped["a"] {
		t.Errorf("a should have run, Skipped=%+v", res.Skipped)
	}
	if mock.CallCount() != 1 {
		t.Errorf("only a should run, got %d LLM calls", mock.CallCount())
	}
	if res.Last == "" {
		t.Errorf("Last should fall back to a's output, got empty")
	}
}

// TestRun_ConditionTrue_BehavesLikeUnconditional: a satisfied guard fires the
// edge, so the downstream chain runs exactly as it would with no guard.
func TestRun_ConditionTrue_BehavesLikeUnconditional(t *testing.T) {
	src := []byte(`---
roles:
  a: {}
  b: {}
edges:
  - a -> b if '{{ contains .a.output "ok" }}'
---

## a

A: {{ .Args }}

## b

{{ .a.output }}
`)
	wf, err := Parse("condtrue.md", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "status ok"})
	mock.Push(llmtest.Script{Text: "B-RAN"})

	res, err := Run(context.Background(), wf, "x", runDeps(t, fakeProvider(mock, 1), bus.New()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Skipped["b"] {
		t.Errorf("b should have run, Skipped=%+v", res.Skipped)
	}
	if res.Outputs["b"] != "B-RAN" {
		t.Errorf("b output = %q", res.Outputs["b"])
	}
}

// TestRun_Skip_NoGoroutineLeak verifies skipped roles spawn no goroutine: a
// large fan of conditionally-skipped roles must leave the goroutine count
// where it started.
func TestRun_Skip_NoGoroutineLeak(t *testing.T) {
	// a is the root; it skips every downstream branch via a false guard.
	src := []byte(`---
roles:
  a: {}
  b: {}
  c: {}
  d: {}
edges:
  - a -> b if '{{ eq .a.verdict "go" }}'
  - a -> c if '{{ eq .a.verdict "go" }}'
  - a -> d if '{{ eq .a.verdict "go" }}'
---

## a

A: {{ .Args }}

## b

{{ .a.output }}

## c

{{ .a.output }}

## d

{{ .a.output }}
`)
	wf, err := Parse("skipleak.md", src)
	if err != nil {
		t.Fatal(err)
	}

	mock := llmtest.NewT(t)
	mock.Push(llmtest.Script{Text: "no\n```json\n{\"verdict\":\"stop\"}\n```"})

	start := goroutineCount()
	res, err := Run(context.Background(), wf, "x", runDeps(t, fakeProvider(mock, 4), bus.New()))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !res.Skipped["b"] || !res.Skipped["c"] || !res.Skipped["d"] {
		t.Errorf("b/c/d should all be skipped, Skipped=%+v", res.Skipped)
	}
	if mock.CallCount() != 1 {
		t.Errorf("only a should run, got %d", mock.CallCount())
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if goroutineCount()-start < 3 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if delta := goroutineCount() - start; delta >= 3 {
		t.Errorf("goroutine delta=%d after 3 skipped roles — skips should spawn none", delta)
	}
}
