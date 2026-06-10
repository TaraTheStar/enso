// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package daemon

import (
	"testing"
	"time"

	"github.com/TaraTheStar/enso/internal/llm/llmtest"
)

// subCount reads the subscriber-slot count for a session under its lock.
func subCount(st *sessionState) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.subs)
}

// waitForSubs polls until the session has exactly `want` subscriber
// slots, failing the test on timeout.
func waitForSubs(t *testing.T, st *sessionState, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if subCount(st) == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waitForSubs: session has %d subscriber slots, want %d", subCount(st), want)
}

// TestDaemon_SubscriberDisconnectReleasesSlot locks in the fix for the
// idle-session subscriber leak: a client that subscribes and then drops
// the connection must have its slot removed promptly — NOT lazily on
// the next event's write failure, because an idle session may never
// produce another event. A leaked slot also keeps proxyPermission's
// hasSubs check true, fanning permission prompts to nobody and denying
// only after the full timeout instead of immediately.
func TestDaemon_SubscriberDisconnectReleasesSlot(t *testing.T) {
	socketPath, mock, srv := startTestServer(t)
	mock.Push(llmtest.Script{Text: "ok"})

	control := dial(t, socketPath)
	info, err := control.CreateSession(CreateSessionReq{
		Prompt: "hello",
		Cwd:    t.TempDir(),
		Yolo:   true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	state := srv.lookup(info.ID)
	if state == nil {
		t.Fatalf("session %q not registered", info.ID)
	}

	// Let the turn finish before subscribing so the session is idle —
	// the leak scenario is precisely "no further events ever arrive".
	waitForScripts(t, mock, 1)

	streamConn := dial(t, socketPath)
	events, err := streamConn.Subscribe(SubscribeReq{SessionID: info.ID, FromSeq: 0})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	waitForSubs(t, state, 1)

	// Drain the ring replay in the background so the client's read loop
	// doesn't matter; then drop the connection with the session idle.
	go func() {
		for range events {
		}
	}()
	_ = streamConn.Close()

	// The server must notice the disconnect without any event traffic
	// and release the slot.
	waitForSubs(t, state, 0)

	// Repeated attach/detach must not accumulate slots either.
	for i := 0; i < 3; i++ {
		c := dial(t, socketPath)
		ev, err := c.Subscribe(SubscribeReq{SessionID: info.ID, FromSeq: 0})
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		waitForSubs(t, state, 1)
		go func() {
			for range ev {
			}
		}()
		_ = c.Close()
		waitForSubs(t, state, 0)
	}
}
