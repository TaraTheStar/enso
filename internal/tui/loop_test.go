// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rivo/tview"
)

// TestLoopCmdFiresAndStops drives the slash command with a 50ms
// interval, confirms it submits the prompt at least twice, then stops
// cleanly via `/loop off`.
func TestLoopCmdFiresAndStops(t *testing.T) {
	var fires int32
	sc := &slashContext{
		chat: tview.NewTextView(),
		submit: func(text string, allowed []string) {
			atomic.AddInt32(&fires, 1)
		},
	}
	c := &loopCmd{sc: sc}

	// Ride the 5-second floor by reaching past it via a low-level start.
	// We can't pass `5s` to Run because that floor blocks short-test
	// intervals; the Run guard is by design. So drive run() directly.
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	go c.run(ctx, 50*time.Millisecond, "check deploy")

	// Wait for at least 2 fires within a generous window.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&fires) < 2 {
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&fires) < 2 {
		t.Fatalf("expected ≥2 fires, got %d", atomic.LoadInt32(&fires))
	}

	if !c.stop() {
		t.Errorf("stop() should report a running loop")
	}
	stopped := atomic.LoadInt32(&fires)

	// Confirm no further fires after stop.
	time.Sleep(150 * time.Millisecond)
	if atomic.LoadInt32(&fires) != stopped {
		t.Errorf("loop kept firing after stop: %d → %d", stopped, atomic.LoadInt32(&fires))
	}
}

func TestLoopCmdRejectsTooShort(t *testing.T) {
	sc := &slashContext{chat: tview.NewTextView(), submit: func(string, []string) {}}
	c := &loopCmd{sc: sc}
	if err := c.Run(context.Background(), "1s do thing"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		t.Errorf("interval below 5s should not start a loop")
	}
}

func TestLoopCmdParsesArgs(t *testing.T) {
	sc := &slashContext{chat: tview.NewTextView(), submit: func(string, []string) {}}
	c := &loopCmd{sc: sc}
	defer c.stop()
	if err := c.Run(context.Background(), "10s ping the deploy"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	running := c.cancel != nil
	c.mu.Unlock()
	if !running {
		t.Errorf("expected loop to be running")
	}
	// Read back the chat output to confirm the wire-up message.
	if got := sc.chat.GetText(true); !strings.Contains(got, "loop") {
		t.Errorf("expected status line, got %q", got)
	}
}

func TestLoopCmdOffStops(t *testing.T) {
	sc := &slashContext{chat: tview.NewTextView(), submit: func(string, []string) {}}
	c := &loopCmd{sc: sc}
	if err := c.Run(context.Background(), "10s some prompt"); err != nil {
		t.Fatal(err)
	}
	if err := c.Run(context.Background(), "off"); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		t.Errorf("/loop off should clear the cancel func")
	}
}
