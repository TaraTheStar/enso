// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// waitQueued blocks until p.Queued() == n or the deadline elapses, so
// tests can enqueue waiters in a deterministic order without sleeps.
func waitQueued(t *testing.T, p *Pool, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for p.Queued() != n {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for Queued()==%d (got %d)", n, p.Queued())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestPool_ConcurrencyBound(t *testing.T) {
	p := NewPool(2)
	r1, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r2, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if p.Inflight() != 2 {
		t.Fatalf("Inflight=%d want 2", p.Inflight())
	}

	got := make(chan struct{})
	go func() {
		r3, _ := p.Acquire(context.Background())
		close(got)
		r3()
	}()
	waitQueued(t, p, 1)
	select {
	case <-got:
		t.Fatal("third Acquire should have blocked at capacity")
	case <-time.After(20 * time.Millisecond):
	}

	r1()
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("third Acquire did not proceed after a release")
	}
	r2()
}

func TestPool_FIFOOrder(t *testing.T) {
	p := NewPool(1)
	hold, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	const n = 5
	order := make(chan int, n)
	releases := make(chan func(), n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			r, _ := p.Acquire(context.Background())
			order <- i
			releases <- r
		}()
		// Enqueue strictly in order: wait until this waiter is parked
		// before launching the next.
		waitQueued(t, p, int64(i+1))
	}

	hold()
	for want := 0; want < n; want++ {
		select {
		case got := <-order:
			if got != want {
				t.Fatalf("FIFO violated: position %d served waiter %d", want, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("waiter for position %d never ran", want)
		}
		(<-releases)()
	}
}

func TestPool_QueueTimeout(t *testing.T) {
	p := NewPoolNamed("t", 1, 40*time.Millisecond)
	hold, _ := p.Acquire(context.Background())
	defer hold()

	start := time.Now()
	_, err := p.Acquire(context.Background())
	if !errors.Is(err, ErrQueueTimeout) {
		t.Fatalf("err=%v want ErrQueueTimeout", err)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("returned too early (%v) — didn't actually wait", elapsed)
	}
	if p.Queued() != 0 {
		t.Fatalf("timed-out waiter still queued: %d", p.Queued())
	}
}

func TestPool_ContextCancel(t *testing.T) {
	p := NewPool(1)
	hold, _ := p.Acquire(context.Background())
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := p.Acquire(ctx)
		errc <- err
	}()
	waitQueued(t, p, 1)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err=%v want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Acquire did not return")
	}
	if p.Queued() != 0 {
		t.Fatalf("cancelled waiter still queued: %d", p.Queued())
	}
}

func TestPool_ReleaseIdempotent(t *testing.T) {
	p := NewPool(1)
	r, _ := p.Acquire(context.Background())
	r()
	r() // second call must be a no-op, not a double-free
	if p.Inflight() != 0 {
		t.Fatalf("Inflight=%d want 0 after release", p.Inflight())
	}
	// A fresh acquire still respects capacity 1.
	r2, _ := p.Acquire(context.Background())
	if p.Inflight() != 1 {
		t.Fatalf("Inflight=%d want 1", p.Inflight())
	}
	r2()
}

// A burst of concurrent acquire/release on a small pool must never
// exceed capacity (run with -race).
func TestPool_NeverExceedsCapacity(t *testing.T) {
	const cap = 3
	p := NewPool(cap)
	var mu sync.Mutex
	cur, max := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := p.Acquire(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			cur++
			if cur > max {
				max = cur
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
			cur--
			mu.Unlock()
			r()
		}()
	}
	wg.Wait()
	if max > cap {
		t.Fatalf("observed %d concurrent holders, capacity was %d", max, cap)
	}
	if p.Inflight() != 0 {
		t.Fatalf("Inflight=%d want 0 at end", p.Inflight())
	}
}
