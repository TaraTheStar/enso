// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"context"
	"sync"
	"sync/atomic"
)

// Pool limits concurrent LLM requests with FIFO ordering.
type Pool struct {
	sem      chan struct{}
	inflight atomic.Int64
	queued   atomic.Int64
	mu       sync.Mutex
}

// NewPool creates a pool that allows up to size concurrent requests.
func NewPool(size int) *Pool {
	if size < 1 {
		size = 1
	}
	return &Pool{
		sem: make(chan struct{}, size),
	}
}

// Acquire blocks until a slot is available, then returns a release function.
func (p *Pool) Acquire(ctx context.Context) (release func(), err error) {
	p.queued.Add(1)

	select {
	case p.sem <- struct{}{}:
		p.queued.Add(-1)
		p.inflight.Add(1)
		return func() {
			p.inflight.Add(-1)
			<-p.sem
		}, nil
	case <-ctx.Done():
		p.queued.Add(-1)
		return func() {}, ctx.Err()
	}
}

// Inflight returns the number of currently executing requests.
func (p *Pool) Inflight() int64 {
	return p.inflight.Load()
}

// Queued returns the number of requests waiting for a slot.
func (p *Pool) Queued() int64 {
	return p.queued.Load()
}
