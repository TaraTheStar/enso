// SPDX-License-Identifier: AGPL-3.0-or-later

package backend

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// recordingChannel records the order of Send calls without blocking
// (unless err is set, in which case Send fails). Recv/Close are inert.
type recordingChannel struct {
	mu    sync.Mutex
	order []MsgKind
	err   error
}

func (c *recordingChannel) Send(env Envelope) error {
	if c.err != nil {
		return c.err
	}
	c.mu.Lock()
	c.order = append(c.order, env.Kind)
	c.mu.Unlock()
	return nil
}
func (c *recordingChannel) Recv() (Envelope, error) {
	return Envelope{}, errors.New("recordingChannel: no Recv")
}
func (c *recordingChannel) Close() error { return nil }

func (c *recordingChannel) kinds() []MsgKind {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]MsgKind(nil), c.order...)
}

// gatedChannel signals each Send as it begins (started) then blocks until
// the test releases it (proceed). Lets a test pin the writer mid-write so
// lane ordering is deterministic.
type gatedChannel struct {
	started chan MsgKind
	proceed chan struct{}
}

func (c *gatedChannel) Send(env Envelope) error {
	c.started <- env.Kind
	<-c.proceed
	return nil
}
func (c *gatedChannel) Recv() (Envelope, error) {
	return Envelope{}, errors.New("gatedChannel: no Recv")
}
func (c *gatedChannel) Close() error { return nil }

// TestQueueWriter_PriorityOvertakesNormal is the core of findings #2/#3:
// a control-plane envelope queued AFTER a backlog of bulk data-plane
// envelopes must still be written before them once the writer is free.
func TestQueueWriter_PriorityOvertakesNormal(t *testing.T) {
	ch := &gatedChannel{started: make(chan MsgKind), proceed: make(chan struct{})}
	w := NewQueueWriter(ch)

	// First data-plane send: the writer takes it and blocks in ch.Send.
	if err := w.Send(Envelope{Kind: MsgInferenceEvent}); err != nil {
		t.Fatalf("Send A: %v", err)
	}
	if k := <-ch.started; k != MsgInferenceEvent {
		t.Fatalf("first write = %q, want %q", k, MsgInferenceEvent)
	}

	// With the writer pinned, enqueue more bulk data and — last — one
	// control envelope. It should still overtake the queued bulk data.
	for _, k := range []MsgKind{MsgEvent, MsgTelemetry} {
		if err := w.Send(Envelope{Kind: k}); err != nil {
			t.Fatalf("Send %q: %v", k, err)
		}
	}
	if err := w.Send(Envelope{Kind: MsgCancel}); err != nil { // priority
		t.Fatalf("Send cancel: %v", err)
	}

	got := []MsgKind{MsgInferenceEvent}
	ch.proceed <- struct{}{} // release the pinned write
	for range 3 {
		got = append(got, <-ch.started)
		ch.proceed <- struct{}{}
	}

	want := []MsgKind{MsgInferenceEvent, MsgCancel, MsgEvent, MsgTelemetry}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("write order = %v, want %v", got, want)
		}
	}
	_ = w.Close()
}

// TestQueueWriter_PreservesNormalOrder confirms same-lane FIFO: the
// data-plane stream (persist/inference) is never reordered relative to
// itself.
func TestQueueWriter_PreservesNormalOrder(t *testing.T) {
	ch := &recordingChannel{}
	w := NewQueueWriter(ch)
	seq := []MsgKind{MsgPersistMessage, MsgPersistMessageUsage, MsgPersistToolCall, MsgEvent, MsgInferenceRequest}
	for _, k := range seq {
		if err := w.Send(Envelope{Kind: k}); err != nil {
			t.Fatalf("Send %q: %v", k, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := ch.kinds()
	if len(got) != len(seq) {
		t.Fatalf("wrote %d envelopes, want %d (%v)", len(got), len(seq), got)
	}
	for i := range seq {
		if got[i] != seq[i] {
			t.Fatalf("order = %v, want %v", got, seq)
		}
	}
}

// TestQueueWriter_FlushesOnClose: everything enqueued before Close is
// written (Close drains the queue before stopping).
func TestQueueWriter_FlushesOnClose(t *testing.T) {
	ch := &recordingChannel{}
	w := NewQueueWriter(ch)
	const n = 500
	for range n {
		_ = w.Send(Envelope{Kind: MsgEvent})
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := len(ch.kinds()); got != n {
		t.Fatalf("flushed %d envelopes, want %d", got, n)
	}
}

// TestQueueWriter_SendAfterClose returns ErrWriterClosed rather than
// hanging or panicking — a lingering producer goroutine must unwind.
func TestQueueWriter_SendAfterClose(t *testing.T) {
	w := NewQueueWriter(&recordingChannel{})
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Send(Envelope{Kind: MsgEvent}) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrWriterClosed) {
			t.Fatalf("Send after Close = %v, want ErrWriterClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send after Close hung")
	}
}

// TestQueueWriter_SendSyncSurfacesErrorSynchronously is the fail-closed
// contract: unlike Send, SendSync returns the real write error on the
// FIRST call, so permission/capability requests and persistence don't
// silently default open or drop a row when the Channel is dead.
func TestQueueWriter_SendSyncSurfacesErrorSynchronously(t *testing.T) {
	boom := errors.New("pipe broken")
	w := NewQueueWriter(&recordingChannel{err: boom})
	defer w.Close()
	if err := w.SendSync(Envelope{Kind: MsgPermissionRequest}); !errors.Is(err, boom) {
		t.Fatalf("SendSync (first call) = %v, want %v", err, boom)
	}
}

// TestQueueWriter_SendSyncReturnsNilOnSuccess confirms the happy path:
// SendSync blocks until the envelope is written, then returns nil.
func TestQueueWriter_SendSyncReturnsNilOnSuccess(t *testing.T) {
	ch := &recordingChannel{}
	w := NewQueueWriter(ch)
	defer w.Close()
	if err := w.SendSync(Envelope{Kind: MsgCapabilityRequest}); err != nil {
		t.Fatalf("SendSync = %v, want nil", err)
	}
	if got := ch.kinds(); len(got) != 1 || got[0] != MsgCapabilityRequest {
		t.Fatalf("recorded %v, want one MsgCapabilityRequest", got)
	}
}

// TestQueueWriter_SurfacesWriteError: a broken pipe stops the writer and
// the error reaches callers via Close and subsequent Send.
func TestQueueWriter_SurfacesWriteError(t *testing.T) {
	boom := errors.New("pipe broken")
	w := NewQueueWriter(&recordingChannel{err: boom})
	_ = w.Send(Envelope{Kind: MsgEvent}) // triggers the failing write
	if err := w.Close(); !errors.Is(err, boom) {
		t.Fatalf("Close = %v, want %v", err, boom)
	}
	if err := w.Send(Envelope{Kind: MsgEvent}); !errors.Is(err, boom) {
		t.Fatalf("Send after error = %v, want %v", err, boom)
	}
}
