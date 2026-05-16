// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrQueueTimeout is returned by Acquire when a request waited longer
// than the pool's queue_timeout without a slot opening. The caller
// (the agent turn / summarise path) surfaces it to the model as a
// tool/chat error so it can retry, pick another model, or give up.
var ErrQueueTimeout = errors.New("pool: queue timeout waiting for a slot")

// Pool bounds concurrent LLM requests across every provider assigned to
// it (a "pool" is the shared-hardware unit — e.g. one llama-swap behind
// one GPU). Waiters are granted slots in strict FIFO arrival
// order via direct hand-off: a releaser passes its slot to the
// longest-waiting live waiter rather than bumping a counter, so a burst
// of Acquire calls can't starve an early arrival.
//
// The zero value is unusable; construct with NewPool / NewPoolNamed.
type Pool struct {
	// Name is the resolved pool name (explicit [pools.X] or an
	// auto-<host>-<port> group). Empty for ad-hoc pools in tests.
	Name string

	mu       sync.Mutex
	capacity int
	inflight int
	queue    []*waiter
	// timeout bounds how long Acquire waits for a slot. 0 = wait
	// indefinitely (until ctx is cancelled) — the behaviour every
	// pre-pools call site relied on.
	timeout time.Duration
}

// waiter is one parked Acquire. ch is buffered(1) so release() can hand
// off a slot without blocking even if the waiter is racing toward a
// cancel/timeout exit. done is set (under Pool.mu) by whichever of
// release/abandon reaches the waiter first, making the hand-off and the
// give-up mutually exclusive.
type waiter struct {
	ch   chan struct{}
	done bool
}

// NewPool creates an unnamed pool allowing up to size concurrent
// requests with no queue timeout (wait until ctx cancel). Retained as
// the test/ad-hoc constructor; the config path uses NewPoolNamed.
func NewPool(size int) *Pool { return NewPoolNamed("", size, 0) }

// NewPoolNamed creates a named pool. size < 1 clamps to 1. timeout <= 0
// disables the queue timeout (callers wait until their ctx is done).
func NewPoolNamed(name string, size int, timeout time.Duration) *Pool {
	if size < 1 {
		size = 1
	}
	if timeout < 0 {
		timeout = 0
	}
	return &Pool{Name: name, capacity: size, timeout: timeout}
}

// Acquire blocks until a slot is free (granted in FIFO order), the
// pool's queue_timeout elapses (ErrQueueTimeout), or ctx is cancelled
// (ctx.Err()). On success it returns a release func that must be called
// exactly once; the returned func is idempotent and safe to defer.
func (p *Pool) Acquire(ctx context.Context) (release func(), err error) {
	p.mu.Lock()
	// Invariant: a non-empty queue implies inflight == capacity.
	// Acquire only appends to the queue under this same lock when
	// inflight == capacity, and release() decrements inflight ONLY when
	// the queue is empty (otherwise it transfers the slot). So this fast
	// path can never fire while waiters are parked — a fresh caller
	// can't barge ahead of the FIFO queue.
	if p.inflight < p.capacity {
		p.inflight++
		p.mu.Unlock()
		return p.releaser(), nil
	}
	w := &waiter{ch: make(chan struct{}, 1)}
	p.queue = append(p.queue, w)
	p.mu.Unlock()

	var timeout <-chan time.Time
	if p.timeout > 0 {
		t := time.NewTimer(p.timeout)
		defer t.Stop()
		timeout = t.C
	}

	select {
	case <-w.ch:
		// release() handed us its slot (inflight already accounts for
		// it — it was transferred, not re-incremented).
		return p.releaser(), nil
	case <-ctx.Done():
		return p.abandon(w, ctx.Err())
	case <-timeout:
		return p.abandon(w, ErrQueueTimeout)
	}
}

// abandon removes a giving-up waiter. If release() already marked it
// done (a slot was handed off in the same instant the waiter timed
// out/cancelled), the slot would otherwise leak — so we take it and
// immediately release it to the next waiter, then still report err.
func (p *Pool) abandon(w *waiter, err error) (func(), error) {
	p.mu.Lock()
	if w.done {
		p.mu.Unlock()
		p.release() // pass the handed-off slot along; don't leak it
		return func() {}, err
	}
	w.done = true
	for i, x := range p.queue {
		if x == w {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			break
		}
	}
	p.mu.Unlock()
	return func() {}, err
}

// releaser returns a single-shot release closure.
func (p *Pool) releaser() func() {
	var once sync.Once
	return func() { once.Do(p.release) }
}

// release frees one slot: either hand it to the oldest live waiter
// (FIFO, slot transferred without touching the inflight count) or, when
// nobody is waiting, decrement inflight.
func (p *Pool) release() {
	p.mu.Lock()
	for len(p.queue) > 0 {
		w := p.queue[0]
		p.queue = p.queue[1:]
		if w.done {
			continue // already abandoned; keep looking
		}
		w.done = true
		p.mu.Unlock()
		w.ch <- struct{}{} // buffered(1) — never blocks
		return
	}
	p.inflight--
	p.mu.Unlock()
}

// Inflight returns the number of currently executing requests.
func (p *Pool) Inflight() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64(p.inflight)
}

// Queued returns the number of requests waiting for a slot.
func (p *Pool) Queued() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return int64(len(p.queue))
}
