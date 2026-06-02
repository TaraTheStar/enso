// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/TaraTheStar/enso/internal/permissions"
)

// viewText renders the model and returns the ANSI-stripped content.
func viewText(m *model) string {
	return ansi.Strip(m.View().Content)
}

// TestInterruptHint_Surfaced is the U3 regression: turn-cancel works
// (double-Esc / Ctrl-C) but used to be invisible. While a turn is in
// flight and the input line is empty, the status line must advertise
// "esc to interrupt"; once the chord is armed it becomes "press esc
// again to stop".
func TestInterruptHint_Surfaced(t *testing.T) {
	m := &model{busy: true, cancelTurn: func() {}}

	if got := viewText(m); !strings.Contains(got, "esc to interrupt") {
		t.Fatalf("busy view should advertise the interrupt affordance; got:\n%s", got)
	}

	// Armed chord (first Esc tapped): the follow-through hint replaces
	// the bare one.
	m.lastEscAt = time.Now()
	got := viewText(m)
	if !strings.Contains(got, "press esc again to stop") {
		t.Fatalf("armed chord should show the follow-through hint; got:\n%s", got)
	}
	if strings.Contains(got, "esc to interrupt") {
		t.Fatalf("armed chord should not also show the bare hint; got:\n%s", got)
	}
}

// TestInterruptHint_SuppressedWhenIdle: no hint when nothing is in
// flight, when the input line has text (Esc clears it there), or when a
// permission prompt owns Esc (= deny).
func TestInterruptHint_SuppressedWhenIdle(t *testing.T) {
	// Idle.
	idle := &model{cancelTurn: func() {}}
	if got := viewText(idle); strings.Contains(got, "esc to interrupt") {
		t.Fatalf("idle view should not advertise interrupt; got:\n%s", got)
	}

	// Busy but the user is typing — Esc clears the buffer, not interrupt.
	typing := &model{busy: true, cancelTurn: func() {}}
	typing.input.buf = "draft message"
	if got := viewText(typing); strings.Contains(got, "esc to interrupt") {
		t.Fatalf("non-empty input should not advertise interrupt; got:\n%s", got)
	}

	// Busy with a pending permission prompt — Esc means deny there.
	prompting := &model{busy: true, cancelTurn: func() {}}
	prompting.perm = &permPending{req: &permissions.PromptRequest{ToolName: "bash"}}
	if got := viewText(prompting); strings.Contains(got, "esc to interrupt") {
		t.Fatalf("pending perm prompt should not advertise interrupt; got:\n%s", got)
	}
}
