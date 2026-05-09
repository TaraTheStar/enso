// SPDX-License-Identifier: AGPL-3.0-or-later

package bus

import (
	"sync"
	"testing"
	"time"
)

// TestPublish_DoesNotBlockOnFullSubscriber locks in the load-bearing
// invariant from AGENTS.md's slow-consumer-drops note: when a
// subscriber's buffer fills, Publish must drop the event rather than
// block the publishing goroutine. The agent's turn loop is the
// publisher; if Publish ever blocks, the entire chat loop stalls
// behind whichever subscriber happens to be slow.
//
// The bus implementation uses `select { case ch <- evt: default: }`
// at bus.go:74. This test guards against a future change to a
// blocking send (which would silently turn streaming-delta lag into
// a full chat freeze).
func TestPublish_DoesNotBlockOnFullSubscriber(t *testing.T) {
	b := New()
	// Capacity 1, then never drained — the next send beyond that
	// would block under any non-default-case implementation.
	b.Subscribe(1)

	const events = 1000
	done := make(chan struct{})
	go func() {
		for i := 0; i < events; i++ {
			b.Publish(Event{Type: EventAssistantDelta, Payload: "x"})
		}
		close(done)
	}()

	select {
	case <-done:
		// Publisher finished even though the subscriber buffer filled
		// almost immediately. That's the desired behaviour.
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a full subscriber — a slow consumer would now stall the agent loop")
	}
}

// TestPublish_ContinuesToOtherSubscribersWhenOneIsFull guards the
// related invariant: a slow subscriber must NOT starve the others.
// In the TUI, both the chat-render subscriber and the audit-log
// subscriber receive the same events; if one fills its buffer, the
// other should still get its messages.
func TestPublish_ContinuesToOtherSubscribersWhenOneIsFull(t *testing.T) {
	b := New()

	slow := b.Subscribe(1)   // we won't drain this
	fast := b.Subscribe(100) // we will drain this and count

	var got int
	var mu sync.Mutex
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for evt := range fast {
			_ = evt
			mu.Lock()
			got++
			mu.Unlock()
		}
	}()

	const events = 50
	for i := 0; i < events; i++ {
		b.Publish(Event{Type: EventAssistantDelta, Payload: "x"})
	}

	// Close the bus so the fast consumer's range loop exits.
	b.Close()
	<-consumerDone

	mu.Lock()
	defer mu.Unlock()
	if got != events {
		t.Errorf("fast subscriber received %d/%d events — slow subscriber starved it", got, events)
	}
	// Sanity-check: slow buffer hit its cap (received exactly 1).
	if len(slow) != 1 {
		t.Errorf("slow buffer len=%d, want 1 (subscriber should have received exactly the first event)", len(slow))
	}
}

// TestPublish_DropDoesNotPanicOnUnknownEventType keeps the slow-
// consumer path safe even if the event-type switch grows a new
// constant whose name lookup hasn't been wired yet. Drop logging
// uses eventTypeString; an "unknown" string is fine, a panic isn't.
func TestPublish_DropDoesNotPanicOnUnknownEventType(t *testing.T) {
	b := New()
	b.Subscribe(0) // zero-cap → every send hits default → every event drops

	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Publish panicked on drop: %v", r)
		}
	}()
	// Use an event type past the defined constants. The drop logger
	// should produce "Unknown" via eventTypeString's default branch
	// without panicking.
	b.Publish(Event{Type: EventType(9999), Payload: nil})
}
