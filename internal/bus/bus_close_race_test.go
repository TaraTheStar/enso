// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"sync"
	"testing"
)

// TestPublish_ConcurrentWithClose locks in the shutdown invariant: a
// Publish racing Close must never panic with "send on closed channel".
// This race is real, not theoretical — background bash jobs' pipe-copy
// goroutines outlive Agent.Run (KillAll only SIGKILLs the group), and
// their final output flush publishes via progressWriter while the host
// (worker adapter, enso run, the TUI) closes the bus immediately after
// the run returns. Run with -race: this exercises Publish vs Close vs
// Subscribe concurrently across many iterations.
func TestPublish_ConcurrentWithClose(t *testing.T) {
	const (
		iterations  = 100
		publishers  = 8
		subscribers = 4
		eventsEach  = 50
	)
	for i := 0; i < iterations; i++ {
		b := New()

		var drained sync.WaitGroup
		for j := 0; j < subscribers; j++ {
			ch := b.Subscribe(1)
			drained.Add(1)
			go func() {
				defer drained.Done()
				for range ch {
				}
			}()
		}

		start := make(chan struct{})
		var wg sync.WaitGroup
		for j := 0; j < publishers; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for k := 0; k < eventsEach; k++ {
					b.Publish(Event{Type: EventAssistantDelta, Payload: "x"})
				}
			}()
		}
		// One late subscriber racing Close alongside the publishers.
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ch := b.Subscribe(1)
			drained.Add(1)
			go func() {
				defer drained.Done()
				for range ch {
				}
			}()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			b.Close()
		}()

		close(start)
		wg.Wait()
		// Every subscriber's range loop must terminate: channels handed
		// out before Close get closed by it, and a Subscribe that loses
		// the race returns an already-closed channel.
		drained.Wait()
	}
}

// TestBus_PublishAndCloseAfterClose verifies the post-shutdown
// semantics Close relies on: Publish no-ops (no panic), a second Close
// is idempotent, and Subscribe returns an already-closed channel so a
// late subscriber's range loop exits instead of hanging.
func TestBus_PublishAndCloseAfterClose(t *testing.T) {
	b := New()
	ch := b.Subscribe(4)
	b.Close()

	if _, ok := <-ch; ok {
		t.Error("pre-Close subscriber channel not closed by Close")
	}

	// Must not panic.
	b.Publish(Event{Type: EventAssistantDelta, Payload: "late flush"})
	b.Close()

	late := b.Subscribe(4)
	select {
	case _, ok := <-late:
		if ok {
			t.Error("Subscribe after Close delivered an event; want closed channel")
		}
	default:
		t.Error("Subscribe after Close returned an open channel; a late subscriber would hang forever")
	}
}
