// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"testing"

	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/bus"
)

func TestPartialTagSuffix(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		// Real partial tag prefixes
		{"foo<", 1},
		{"foo<t", 2},
		{"foo<th", 3},
		{"foo<think", 6},
		{"foo</thi", 5},
		{"foo</think", 7}, // 7 of 8 — full tag still incomplete by `>`
		// Full tag — no buffering needed
		{"foo<think>", 0},
		{"foo</think>", 0},
		// No partial
		{"foo bar", 0},
		{"hello", 0},
		// Edge: just `<` at end
		{"<", 1},
		// Edge: empty string
		{"", 0},
		// Bytes that look tag-ish but aren't a prefix
		{"foo<x", 0},
	}
	for _, tc := range cases {
		if got := partialTagSuffix(tc.in); got != tc.want {
			t.Errorf("partialTagSuffix(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestEventInputDiscarded_PluralizedRender(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{Type: bus.EventInputDiscarded, Payload: 3})
	out := c.view.GetText(true)
	if !strings.Contains(out, "3 messages discarded") {
		t.Errorf("missing plural notice: %q", out)
	}
	if !strings.Contains(out, "after cancel") {
		t.Errorf("missing 'after cancel' framing: %q", out)
	}
}

func TestEventInputDiscarded_SingularPhrasing(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{Type: bus.EventInputDiscarded, Payload: 1})
	out := c.view.GetText(true)
	if !strings.Contains(out, "1 message discarded") {
		t.Errorf("expected singular form for count=1: %q", out)
	}
	if strings.Contains(out, "1 messages") {
		t.Errorf("singular noun expected for count=1: %q", out)
	}
}

func TestEventInputDiscarded_DaemonFloatPayload(t *testing.T) {
	// Daemon-attached events arrive as float64 after JSON round-trip.
	// The chat handler must accept that shape too.
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{Type: bus.EventInputDiscarded, Payload: float64(2)})
	out := c.view.GetText(true)
	if !strings.Contains(out, "2 messages discarded") {
		t.Errorf("daemon-shape payload not rendered: %q", out)
	}
}

func TestEventInputDiscarded_ZeroCountSkipped(t *testing.T) {
	// Defensive: an accidental count=0 should produce nothing rather
	// than rendering "0 messages discarded" (visually misleading).
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{Type: bus.EventInputDiscarded, Payload: 0})
	out := c.view.GetText(true)
	if strings.Contains(out, "discarded") {
		t.Errorf("zero count should produce no notice: %q", out)
	}
}
