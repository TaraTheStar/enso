// SPDX-License-Identifier: AGPL-3.0-or-later

package workflow

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm/llmtest"
)

// TestRun_LargeFanOut covers AGENTS.md's "workflow sibling parallelism
// is goroutine-correct but not load-tested" entry: build a workflow
// with 12 independent sibling roles all converging on a single
// reducer, run it with mock LLM scripts, and assert every sibling
// completes without deadlock or output corruption.
//
// The mutex on the runner's shared output state is the shape AGENTS.md
// flags as unexplored at scale; this test exercises it with enough
// concurrent writers that any obvious lock-ordering bug or missed
// synchronisation should surface as a hang or a missing/duplicated
// output entry.
func TestRun_LargeFanOut(t *testing.T) {
	const siblings = 12

	// Build the workflow source dynamically so this test scales by
	// changing the constant rather than hand-editing YAML.
	var src strings.Builder
	src.WriteString("---\nroles:\n")
	for i := 0; i < siblings; i++ {
		fmt.Fprintf(&src, "  s%02d: {}\n", i)
	}
	src.WriteString("  reducer: {}\nedges:\n")
	for i := 0; i < siblings; i++ {
		fmt.Fprintf(&src, "  - s%02d -> reducer\n", i)
	}
	src.WriteString("---\n\n")
	for i := 0; i < siblings; i++ {
		fmt.Fprintf(&src, "## s%02d\n\nSibling %d work: {{ .Args }}\n\n", i, i)
	}
	src.WriteString("## reducer\n\nfinal\n")

	wf, err := Parse("fanout.md", []byte(src.String()))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	mock := llmtest.NewT(t)
	for i := 0; i < siblings; i++ {
		mock.Push(llmtest.Script{Text: fmt.Sprintf("out-%02d", i)})
	}
	mock.Push(llmtest.Script{Text: "FINAL"})

	resCh := make(chan *Result, 1)
	errCh := make(chan error, 1)
	go func() {
		// Pool size = siblings so the runner is allowed to fan out
		// fully. With a smaller pool the test would still pass but
		// would be measuring queueing rather than parallelism.
		r, err := Run(context.Background(), wf, "go",
			runDeps(t, fakeProvider(mock, siblings), bus.New()))
		if err != nil {
			errCh <- err
			return
		}
		resCh <- r
	}()

	// Give a generous deadline — under -race this can be slow because
	// 12 goroutines + a mutex is the kind of thing -race instruments
	// heavily.
	select {
	case res := <-resCh:
		if res.Outputs["reducer"] != "FINAL" {
			t.Errorf("reducer output: %q", res.Outputs["reducer"])
		}
		// Every sibling must have produced an output entry. If the
		// shared-state mutex misbehaved, we'd see missing keys or
		// duplicates.
		seen := map[string]bool{}
		for i := 0; i < siblings; i++ {
			role := fmt.Sprintf("s%02d", i)
			out := res.Outputs[role]
			if out == "" {
				t.Errorf("role %s missing output", role)
				continue
			}
			if seen[out] {
				t.Errorf("duplicate output value %q across siblings — output map is racing", out)
			}
			seen[out] = true
		}
		if mock.CallCount() != siblings+1 {
			t.Errorf("CallCount=%d, want %d (one per sibling + reducer)",
				mock.CallCount(), siblings+1)
		}
	case err := <-errCh:
		t.Fatalf("workflow err: %v", err)
	case <-time.After(15 * time.Second):
		t.Fatal("workflow deadlocked or took too long with 12 siblings")
	}
}

// TestRun_LargeFanOut_NoGoroutineLeak runs the fan-out workflow and
// verifies the goroutine count returns to roughly its starting level
// once Run returns. A leak in the per-role spawn path would compound
// across long sessions; AGENTS.md flags this code as goroutine-correct
// but unproven at scale, so an explicit leak check is worth its line
// count.
func TestRun_LargeFanOut_NoGoroutineLeak(t *testing.T) {
	const siblings = 8

	var src strings.Builder
	src.WriteString("---\nroles:\n")
	for i := 0; i < siblings; i++ {
		fmt.Fprintf(&src, "  s%d: {}\n", i)
	}
	src.WriteString("---\n\n")
	for i := 0; i < siblings; i++ {
		fmt.Fprintf(&src, "## s%d\n\nrun {{ .Args }}\n\n", i)
	}

	wf, err := Parse("leak.md", []byte(src.String()))
	if err != nil {
		t.Fatal(err)
	}

	mock := llmtest.NewT(t)
	for i := 0; i < siblings; i++ {
		mock.Push(llmtest.Script{Text: fmt.Sprintf("done-%d", i)})
	}

	startGoroutines := goroutineCount()
	if _, err := Run(context.Background(), wf, "x",
		runDeps(t, fakeProvider(mock, siblings), bus.New())); err != nil {
		t.Fatalf("run: %v", err)
	}

	// Allow a short grace window for any deferred shutdown. This isn't
	// perfectly tight (the Go runtime keeps some goroutines around for
	// its own bookkeeping) but a real leak of N spawn goroutines per
	// role would still show up as a delta growing with `siblings`.
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if delta := goroutineCount() - startGoroutines; delta < siblings {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	if delta := goroutineCount() - startGoroutines; delta >= siblings {
		t.Errorf("goroutine delta=%d after fan-out of %d siblings — likely leak",
			delta, siblings)
	}
}

var goroutineMu sync.Mutex

func goroutineCount() int {
	// runtime.NumGoroutine is what we want here. Wrapped in a mutex
	// so concurrent test runs don't fight; the count is approximate
	// regardless because of background runtime goroutines.
	goroutineMu.Lock()
	defer goroutineMu.Unlock()
	return runtimeNumGoroutine()
}
