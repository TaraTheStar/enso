// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/bus"
)

func TestFmtToolElapsed(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{500 * time.Millisecond, "0s"}, // sub-second floors to 0s; gating happens at the badge layer
		{2 * time.Second, "2s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m00s"},
		{75 * time.Second, "1m15s"},
		{125 * time.Second, "2m05s"},
	}
	for _, tc := range cases {
		if got := fmtToolElapsed(tc.in); got != tc.want {
			t.Errorf("fmtToolElapsed(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestToolBlockBadge_HiddenUnderThreshold(t *testing.T) {
	// Running but only 1s in: most read/grep/glob calls finish here, so
	// the badge must stay hidden to avoid a flicker on every fast tool.
	b := &toolBlock{startedAt: time.Now().Add(-1 * time.Second)}
	if got := toolBlockBadge(b); got != "" {
		t.Errorf("running 1s should produce no badge, got %q", got)
	}
}

func TestToolBlockBadge_RunningPastThreshold(t *testing.T) {
	b := &toolBlock{startedAt: time.Now().Add(-3 * time.Second)}
	got := toolBlockBadge(b)
	if !strings.Contains(got, "running") {
		t.Errorf("expected 'running' in badge, got %q", got)
	}
	if !strings.Contains(got, "[gray]") {
		t.Errorf("expected gray tag, got %q", got)
	}
}

func TestToolBlockBadge_CompletedShowsFinal(t *testing.T) {
	b := &toolBlock{startedAt: time.Now().Add(-12 * time.Second), duration: 12 * time.Second}
	got := toolBlockBadge(b)
	if strings.Contains(got, "running") {
		t.Errorf("completed badge must not say 'running', got %q", got)
	}
	if !strings.Contains(got, "12s") {
		t.Errorf("expected '12s' in completed badge, got %q", got)
	}
}

func TestToolBlockBadge_FastCompletionNoBadge(t *testing.T) {
	// Completed in 400ms — under finalBadgeThreshold. We don't pollute
	// scrollback with sub-second durations.
	b := &toolBlock{startedAt: time.Now().Add(-400 * time.Millisecond), duration: 400 * time.Millisecond}
	if got := toolBlockBadge(b); got != "" {
		t.Errorf("sub-second completion should produce no badge, got %q", got)
	}
}

func TestToolBlockBadge_ReplayBlocksUnstamped(t *testing.T) {
	// ReplayHistory creates toolBlocks without startedAt. Badge logic
	// must not treat them as either running or completed-with-duration.
	b := &toolBlock{call: "read(path=foo)"}
	if got := toolBlockBadge(b); got != "" {
		t.Errorf("replay block should produce no badge, got %q", got)
	}
}

func TestHasLiveTimerBlock(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")

	if c.HasLiveTimerBlock() {
		t.Error("empty chat should report no live timer")
	}

	// One running block, but only 1s in — under threshold.
	c.blocks = append(c.blocks, &toolBlock{id: "a", startedAt: time.Now().Add(-1 * time.Second)})
	if c.HasLiveTimerBlock() {
		t.Error("under-threshold running block should not request a tick")
	}

	// Past threshold: ticker should fire redraws.
	c.blocks = append(c.blocks, &toolBlock{id: "b", startedAt: time.Now().Add(-3 * time.Second)})
	if !c.HasLiveTimerBlock() {
		t.Error("past-threshold running block should request a tick")
	}

	// Completed blocks don't count even if their final duration is huge.
	c.blocks = []chatBlock{&toolBlock{id: "c", startedAt: time.Now().Add(-30 * time.Second), duration: 30 * time.Second}}
	if c.HasLiveTimerBlock() {
		t.Error("completed block must not request a tick")
	}
}

func TestEventToolCallStart_StampsStartedAt(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")
	before := time.Now()
	c.Render(bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{
		"id":   "tc1",
		"name": "bash",
		"args": map[string]any{"cmd": "ls"},
	}})
	after := time.Now()

	var tb *toolBlock
	for _, b := range c.blocks {
		if t2, ok := b.(*toolBlock); ok && t2.id == "tc1" {
			tb = t2
			break
		}
	}
	if tb == nil {
		t.Fatal("toolBlock not appended on EventToolCallStart")
	}
	if tb.startedAt.Before(before) || tb.startedAt.After(after) {
		t.Errorf("startedAt=%v not in [%v,%v]", tb.startedAt, before, after)
	}
	if tb.duration != 0 {
		t.Errorf("duration should be zero while running, got %v", tb.duration)
	}
}

func TestEventToolCallEnd_StampsDuration(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{
		"id": "tc1", "name": "bash", "args": map[string]any{},
	}})
	// Backdate so duration crosses the final-badge threshold and the
	// End handler does its redraw path (covers the redraw branch too).
	for _, b := range c.blocks {
		if tb, ok := b.(*toolBlock); ok && tb.id == "tc1" {
			tb.startedAt = time.Now().Add(-3 * time.Second)
		}
	}
	c.Render(bus.Event{Type: bus.EventToolCallEnd, Payload: map[string]any{
		"id": "tc1", "name": "bash", "result": "",
	}})

	var tb *toolBlock
	for _, b := range c.blocks {
		if t2, ok := b.(*toolBlock); ok && t2.id == "tc1" {
			tb = t2
			break
		}
	}
	if tb == nil {
		t.Fatal("toolBlock missing after End")
	}
	if tb.duration < 2*time.Second {
		t.Errorf("duration=%v, expected >= 2s", tb.duration)
	}
	if tb.running() {
		t.Errorf("block should report not-running after End")
	}
}
