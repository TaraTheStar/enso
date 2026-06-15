// SPDX-License-Identifier: AGPL-3.0-or-later

package backend

import (
	"errors"
	"sync"
)

// ErrWriterClosed is returned by QueueWriter.Send once the writer
// goroutine has stopped — either because Close was called or because an
// underlying Channel.Send failed (the pipe broke). It lets a blocked
// producer unwind instead of hanging on a queue nobody drains.
var ErrWriterClosed = errors.New("backend: queue writer closed")

const (
	// queuePriorityCap bounds the control-plane lane. Control envelopes
	// (cancel, permission/capability decisions, control RPCs, lifecycle)
	// are low-volume; a small buffer keeps a burst of them from blocking a
	// producer without ever growing large.
	queuePriorityCap = 256
	// queueNormalCap bounds the data-plane lane. Sized to match the bus
	// subscriber buffer (internal/bus) so a streamed-output burst is
	// absorbed by the queue rather than overflowing the bus and dropping
	// events (see finding #2). When it does fill, enqueue blocks — lossless
	// back-pressure for ordered persistence and streamed inference.
	queueNormalCap = 8192
)

// QueueWriter serializes every Channel send through one writer goroutine,
// decoupling producer goroutines from the blocking pipe write at the
// bottom of the host/worker seam.
//
// Two lanes feed the writer. Control-plane envelopes travel a PRIORITY
// lane and overtake bulk data-plane envelopes (streamed inference/bus
// events, telemetry, persistence) already queued; the writer drains the
// priority lane first on every iteration. So a flood of streamed output
// can no longer delay the Cancel or permission decision meant to get
// through — the head-of-line coupling that wedged the seam (findings #2 &
// #3). Reordering only takes effect under back-pressure: when both lanes
// are otherwise empty a lone envelope is written immediately, preserving
// FIFO order in the common case. Envelopes WITHIN a lane keep their order,
// so the persistence and inference-stream sequences (all data-plane) are
// never reordered relative to themselves.
//
// Because one goroutine owns Channel.Send, producers never serialize on a
// mutex held across the pipe syscall; they only enqueue. Send is
// therefore fire-and-forget: it returns once the envelope is queued (or
// the writer has died), and a write error surfaces on a later Send /
// Close rather than synchronously. Callers that need to observe a broken
// seam rely on the correlated reader path tearing down (Recv error →
// ctx cancel), exactly as the persist/event sends already did.
type QueueWriter struct {
	ch       Channel
	priority chan queued
	normal   chan queued

	closeOnce sync.Once
	quit      chan struct{} // closed by Close: drain remaining, then stop
	done      chan struct{} // closed when the writer goroutine has returned

	errMu sync.Mutex
	err   error // first underlying Channel.Send error
}

// queued is one enqueued envelope. res is non-nil only for SendSync: the
// writer signals the real Channel.Send error on it after the write, so a
// caller that must fail-closed on a dead Channel (permission/capability
// requests, persistence) keeps synchronous error semantics.
type queued struct {
	env Envelope
	res chan error
}

// NewQueueWriter wraps ch and starts the single writer goroutine. The
// caller owns ch's lifetime (Close here stops the writer but does not
// close ch — the backend tears the transport down separately).
func NewQueueWriter(ch Channel) *QueueWriter {
	w := &QueueWriter{
		ch:       ch,
		priority: make(chan queued, queuePriorityCap),
		normal:   make(chan queued, queueNormalCap),
		quit:     make(chan struct{}),
		done:     make(chan struct{}),
	}
	go w.run()
	return w
}

// Send enqueues env on its lane and returns once queued (fire-and-forget).
// Control-plane envelopes take the priority lane. Blocks only while the
// chosen lane is full (lossless back-pressure); returns the writer's error
// if it has already stopped. The underlying write error is NOT observed
// here — use SendSync when a caller must fail-closed on a dead Channel.
func (w *QueueWriter) Send(env Envelope) error {
	return w.enqueue(queued{env: env})
}

// SendSync enqueues env and blocks until the writer has actually written
// it, returning the real Channel.Send error. It still rides the single
// writer and the priority lane, so a synchronous permission/capability
// request is not delayed behind queued bulk data. Used where losing the
// send must not silently default open or drop session rows.
func (w *QueueWriter) SendSync(env Envelope) error {
	res := make(chan error, 1)
	if err := w.enqueue(queued{env: env, res: res}); err != nil {
		return err
	}
	select {
	case err := <-res:
		return err
	case <-w.done:
		return w.sendErr()
	}
}

func (w *QueueWriter) enqueue(q queued) error {
	// Check for a stopped writer first: with a free lane slot a bare
	// two-case select could pick the enqueue even after done closed,
	// silently dropping the envelope into a queue nobody drains.
	select {
	case <-w.done:
		return w.sendErr()
	default:
	}
	lane := w.normal
	if controlPlane(q.env.Kind) {
		lane = w.priority
	}
	select {
	case lane <- q:
		return nil
	case <-w.done:
		return w.sendErr()
	}
}

// Close stops the writer goroutine after it drains whatever is already
// queued, then returns the first underlying send error (nil on a clean
// shutdown). Idempotent.
func (w *QueueWriter) Close() error {
	w.closeOnce.Do(func() { close(w.quit) })
	<-w.done
	w.errMu.Lock()
	defer w.errMu.Unlock()
	return w.err
}

func (w *QueueWriter) run() {
	defer close(w.done)
	for {
		// Priority first: a queued control envelope always overtakes
		// pending data-plane traffic.
		select {
		case env := <-w.priority:
			if !w.write(env) {
				return
			}
			continue
		default:
		}
		select {
		case env := <-w.priority:
			if !w.write(env) {
				return
			}
		case env := <-w.normal:
			if !w.write(env) {
				return
			}
		case <-w.quit:
			w.drain()
			return
		}
	}
}

// drain flushes whatever is queued at Close, priority first, best-effort.
// Producers racing the drain may have an envelope dropped — acceptable at
// shutdown, where the run is already over.
func (w *QueueWriter) drain() {
	for {
		select {
		case env := <-w.priority:
			if !w.write(env) {
				return
			}
		default:
			select {
			case env := <-w.normal:
				if !w.write(env) {
					return
				}
			default:
				return
			}
		}
	}
}

// write performs the one blocking pipe send, signals a SendSync waiter
// with the result, and on failure records the first error and stops the
// goroutine (a broken pipe won't recover); returns false to end run().
func (w *QueueWriter) write(q queued) bool {
	err := w.ch.Send(q.env)
	if q.res != nil {
		q.res <- err // buffered (cap 1); never blocks
	}
	if err != nil {
		w.errMu.Lock()
		if w.err == nil {
			w.err = err
		}
		w.errMu.Unlock()
		return false
	}
	return true
}

func (w *QueueWriter) sendErr() error {
	w.errMu.Lock()
	defer w.errMu.Unlock()
	if w.err != nil {
		return w.err
	}
	return ErrWriterClosed
}

// controlPlane reports whether a kind rides the priority lane. These are
// the low-volume, latency-sensitive envelopes whose delivery must not
// queue behind streamed bulk data: lifecycle, cancellation, the
// interactive control RPCs, and the permission/capability hand-offs that
// a blocked tool is waiting on.
func controlPlane(k MsgKind) bool {
	switch k {
	case MsgWorkerReady, MsgWorkerDone, MsgWorkerError,
		MsgShutdown, MsgCancel,
		MsgControlRequest, MsgControlResponse,
		MsgPermissionRequest, MsgPermissionDecision,
		MsgCapabilityRequest, MsgCapabilityGrant,
		MsgInferenceCancel:
		return true
	}
	return false
}
