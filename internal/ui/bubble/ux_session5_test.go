// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/instructions"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// --- Overlay scrolling (picker / palette / sessions) ---

// TestWindowList covers the scrolling viewport math: an unbounded budget
// (or a short list) returns everything with nothing hidden, while a
// bounded budget keeps the selection on-screen and reports the hidden
// counts above/below.
func TestWindowList(t *testing.T) {
	lines := make([]string, 40)
	for i := range lines {
		lines[i] = fmt.Sprintf("L%02d", i)
	}

	// Unknown height (maxRows <= 0): degrade to the full list — this is
	// the bare-model case in unit tests, must not clip to nothing.
	if got, a, b := windowList(lines, 10, 0); len(got) != 40 || a != 0 || b != 0 {
		t.Fatalf("maxRows<=0 should return all; got %d above=%d below=%d", len(got), a, b)
	}
	// List shorter than the budget: all visible.
	if got, a, b := windowList(lines[:5], 0, 10); len(got) != 5 || a != 0 || b != 0 {
		t.Fatalf("short list should return all; got %d above=%d below=%d", len(got), a, b)
	}
	// Selection at the top.
	got, a, b := windowList(lines, 0, 10)
	if len(got) != 10 || got[0] != "L00" || a != 0 || b != 30 {
		t.Fatalf("top window: len=%d first=%q above=%d below=%d", len(got), got[0], a, b)
	}
	// Selection in the middle is roughly centred.
	got, a, b = windowList(lines, 20, 10)
	if len(got) != 10 || a != 15 || b != 15 || got[0] != "L15" {
		t.Fatalf("mid window: len=%d first=%q above=%d below=%d", len(got), got[0], a, b)
	}
	// Selection at the bottom pins the window to the end.
	got, a, b = windowList(lines, 39, 10)
	if len(got) != 10 || a != 30 || b != 0 || got[len(got)-1] != "L39" {
		t.Fatalf("bottom window: len=%d last=%q above=%d below=%d", len(got), got[len(got)-1], a, b)
	}
}

// TestScrollSuffix: the footer tail names hidden rows in each direction,
// and is empty when nothing is hidden.
func TestScrollSuffix(t *testing.T) {
	if s := scrollSuffix(0, 0); s != "" {
		t.Fatalf("nothing hidden should be empty; got %q", s)
	}
	if s := scrollSuffix(5, 0); !strings.Contains(s, "↑5") || strings.Contains(s, "↓") {
		t.Fatalf("above-only suffix wrong: %q", s)
	}
	if s := scrollSuffix(0, 3); !strings.Contains(s, "↓3") || strings.Contains(s, "↑") {
		t.Fatalf("below-only suffix wrong: %q", s)
	}
	if s := scrollSuffix(2, 4); !strings.Contains(s, "↑2") || !strings.Contains(s, "↓4") {
		t.Fatalf("both-direction suffix wrong: %q", s)
	}
}

// TestRenderPicker_Scrolls is the wiring check: a file list taller than
// the terminal renders only a window around the selection (the selected
// file is shown, a scrolled-off file is not) and surfaces the
// "N more" hint in the footer.
func TestRenderPicker_Scrolls(t *testing.T) {
	files := make([]string, 40)
	for i := range files {
		files[i] = fmt.Sprintf("file%02d.go", i)
	}
	p := &pickerData{files: files, walked: true, sel: 35}

	out := ansi.Strip(renderPicker(p, 80, 20))
	if !strings.Contains(out, "file35.go") {
		t.Fatalf("selected file should be visible; got:\n%s", out)
	}
	if strings.Contains(out, "file00.go") {
		t.Fatalf("a scrolled-off file should not be visible; got:\n%s", out)
	}
	if !strings.Contains(out, "more") {
		t.Fatalf("footer should advertise hidden rows; got:\n%s", out)
	}

	// With a tall terminal the whole list fits — no scroll hint.
	full := ansi.Strip(renderPicker(p, 80, 200))
	if !strings.Contains(full, "file00.go") || strings.Contains(full, "more") {
		t.Fatalf("tall terminal should show everything with no hint; got:\n%s", full)
	}
}

// --- @ picker: mention marker + lossless cancel ---

// TestPicker_InsertsMentionMarker: accepting a file inserts an
// `@<path>` mention (not a bare path), mirroring the slash palette's
// `/<name>` so file references read as references.
func TestPicker_InsertsMentionMarker(t *testing.T) {
	m := &model{picker: &pickerData{files: []string{"src/main.go", "README.md"}, walked: true}, pickerOpen: true}
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})

	if m.pickerOpen {
		t.Fatal("Enter should close the picker")
	}
	if m.input.buf != "@src/main.go " {
		t.Fatalf("accept should insert an @-mention with trailing space; buf=%q", m.input.buf)
	}
}

// TestPicker_CancelRestoresKeystroke: the `@` that opened the picker was
// never inserted, so Esc (or Enter with no match) must restore the typed
// `@<filter>` rather than silently dropping the keystroke and any filter
// text.
func TestPicker_CancelRestoresKeystroke(t *testing.T) {
	// Esc with a partial filter.
	m := &model{picker: &pickerData{files: []string{"src/main.go"}, walked: true}, pickerOpen: true}
	m.picker.filter = "src"
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	if m.pickerOpen {
		t.Fatal("Esc should close the picker")
	}
	if m.input.buf != "@src" {
		t.Fatalf("Esc should restore the typed @+filter; buf=%q", m.input.buf)
	}

	// Enter with no match restores the typed text too.
	m = &model{picker: &pickerData{files: []string{"src/main.go"}, walked: true}, pickerOpen: true}
	m.picker.filter = "zzz" // matches nothing
	m.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if m.pickerOpen {
		t.Fatal("Enter with no match should close the picker")
	}
	if m.input.buf != "@zzz" {
		t.Fatalf("no-match Enter should restore the typed @+filter; buf=%q", m.input.buf)
	}
}

// --- Egress prompt auto-deny deadline ---

// TestEgress_DefaultDeadlineApplied: an egress request with no
// broker-set deadline gets the TUI's default so a sealed box never hangs
// on an absent user.
func TestEgress_DefaultDeadlineApplied(t *testing.T) {
	m := &model{}
	req := &permissions.EgressPrompt{Target: "evil.example:443", Respond: make(chan permissions.EgressDecision, 1)}

	before := time.Now()
	m.handleBusEvent(bus.Event{Type: bus.EventEgressRequest, Payload: req})

	if m.egress == nil {
		t.Fatal("egress prompt should be pending")
	}
	if m.egress.deadline.IsZero() {
		t.Fatal("a default deadline must be applied when the broker set none")
	}
	if d := m.egress.deadline.Sub(before); d < 30*time.Second || d > 2*time.Minute {
		t.Fatalf("default deadline %v not in the expected ballpark", d)
	}

	// The status line surfaces the countdown.
	if got := viewText(m); !strings.Contains(got, "auto-deny in") {
		t.Fatalf("View should show the egress auto-deny countdown; got:\n%s", got)
	}
}

// TestEgress_BrokerDeadlineHonoured: a deadline set by the broker is
// used verbatim rather than overwritten by the default.
func TestEgress_BrokerDeadlineHonoured(t *testing.T) {
	m := &model{}
	dl := time.Now().Add(5 * time.Second)
	req := &permissions.EgressPrompt{Target: "x:443", Respond: make(chan permissions.EgressDecision, 1), Deadline: dl}
	m.handleBusEvent(bus.Event{Type: bus.EventEgressRequest, Payload: req})
	if !m.egress.deadline.Equal(dl) {
		t.Fatalf("broker deadline should be honoured; got %v want %v", m.egress.deadline, dl)
	}
}

// TestEgress_TickAutoDenies: once the deadline passes, the countdown
// tick auto-denies (the safe default) and clears the prompt.
func TestEgress_TickAutoDenies(t *testing.T) {
	req := &permissions.EgressPrompt{Target: "x:443", Respond: make(chan permissions.EgressDecision, 1)}
	m := &model{egress: &egressPending{req: req, deadline: time.Now().Add(-time.Second)}}

	m.Update(egressTickMsg{})

	if m.egress != nil {
		t.Fatal("expired prompt should be cleared")
	}
	select {
	case d := <-req.Respond:
		if d != permissions.EgressDeny {
			t.Fatalf("auto-deny should send EgressDeny; got %v", d)
		}
	case <-time.After(time.Second):
		t.Fatal("auto-deny decision was not sent")
	}
}

// --- Idle Ctrl-C / Ctrl-D quit confirmation ---

// TestIdleQuit_RequiresConfirmation: a single idle Ctrl-C (or empty-line
// Ctrl-D) arms a confirmation instead of quitting; a second press
// commits the quit.
func TestIdleQuit_RequiresConfirmation(t *testing.T) {
	for _, key := range []tea.KeyPressMsg{
		{Code: 'c', Mod: tea.ModCtrl},
		{Code: 'd', Mod: tea.ModCtrl},
	} {
		m := &model{}
		m.handleKey(key)
		if m.quitting {
			t.Fatalf("%s: first idle press must not quit", key.String())
		}
		if m.lastQuitAt.IsZero() {
			t.Fatalf("%s: first press should arm the confirmation", key.String())
		}
		m.handleKey(key)
		if !m.quitting {
			t.Fatalf("%s: second press should quit", key.String())
		}
	}
}

// TestIdleQuit_UnrelatedKeyDisarms: a key other than Ctrl-C/Ctrl-D
// between the two presses clears the arm, so quitting needs a fresh
// confirming pair (a stray quit keystroke never stays primed behind
// unrelated typing).
func TestIdleQuit_UnrelatedKeyDisarms(t *testing.T) {
	m := &model{}
	m.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if m.lastQuitAt.IsZero() {
		t.Fatal("first Ctrl-C should arm")
	}
	m.handleKey(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if !m.lastQuitAt.IsZero() {
		t.Fatal("an unrelated key should disarm the quit confirmation")
	}
	m.handleKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if m.quitting {
		t.Fatal("after disarm a single Ctrl-C must not quit")
	}
}

// TestCtrlD_NonEmptyClearsLine: Ctrl-D with text in the buffer still
// clears the line (readline parity) and never quits.
func TestCtrlD_NonEmptyClearsLine(t *testing.T) {
	m := &model{}
	m.input.buf = "half a thought"
	m.input.cursor = len(m.input.buf)
	m.handleKey(tea.KeyPressMsg{Code: 'd', Mod: tea.ModCtrl})
	if m.quitting {
		t.Fatal("Ctrl-D on a non-empty line must not quit")
	}
	if m.input.buf != "" {
		t.Fatalf("Ctrl-D should clear the line; buf=%q", m.input.buf)
	}
}

// --- Live context-% indicator ---

// fakeAgentControl is a minimal agentControl for the status-line
// indicator test: only the token/window accessors carry meaning.
type fakeAgentControl struct {
	est    int
	window int
	cumIn  int64
	cumOut int64
	prov   *llm.Provider
}

func (f *fakeAgentControl) Provider() *llm.Provider                    { return f.prov }
func (f *fakeAgentControl) ProviderCtx() *instructions.ProviderContext { return nil }
func (f *fakeAgentControl) SetProvider(string) error                   { return nil }
func (f *fakeAgentControl) EstimateTokens() int                        { return f.est }
func (f *fakeAgentControl) CumulativeInputTokens() int64               { return f.cumIn }
func (f *fakeAgentControl) CumulativeOutputTokens() int64              { return f.cumOut }
func (f *fakeAgentControl) ContextWindow() int                         { return f.window }
func (f *fakeAgentControl) PrefixBreakdown() agent.PrefixBreakdown     { return agent.PrefixBreakdown{} }
func (f *fakeAgentControl) CompactPreview() agent.CompactPreviewResult {
	return agent.CompactPreviewResult{}
}
func (f *fakeAgentControl) ForceCompact(context.Context) (bool, error) { return false, nil }
func (f *fakeAgentControl) ForcePrune() (int, int, int)                { return 0, 0, 0 }
func (f *fakeAgentControl) SetNextTurnTools([]string)                  {}

// TestContextIndicator: shows "ctx N%" when window data exists, hides
// when the window is unconfigured, and is nil-safe with no agent
// control (true attach mode).
func TestContextIndicator(t *testing.T) {
	// Half-full window.
	m := &model{slashCtx: &slashCtx{agt: &fakeAgentControl{est: 5000, window: 10000}}}
	if got := m.contextIndicator(); !strings.Contains(ansi.Strip(got), "ctx 50%") {
		t.Fatalf("expected ctx 50%%; got %q", ansi.Strip(got))
	}
	// Surfaced in the rendered status line.
	if got := viewText(m); !strings.Contains(got, "ctx 50%") {
		t.Fatalf("status line should carry the context indicator; got:\n%s", got)
	}

	// Unconfigured window → no badge.
	noWin := &model{slashCtx: &slashCtx{agt: &fakeAgentControl{est: 100, window: 0}}}
	if got := noWin.contextIndicator(); got != "" {
		t.Fatalf("unconfigured window should produce no indicator; got %q", got)
	}

	// No agent control (attach mode) → nil-safe, empty.
	if got := (&model{}).contextIndicator(); got != "" {
		t.Fatalf("nil agent control should produce no indicator; got %q", got)
	}
}

// TestCostIndicator: a priced provider shows a "$" dollar badge, a
// free/local provider (no prices) shows a "Σ" cumulative-token badge, a
// fresh session (no tokens billed) shows nothing, and it's nil-safe in
// true attach mode.
func TestCostIndicator(t *testing.T) {
	// Priced provider: 1M input @ $3/M + 500k output @ $6/M = $3 + $3 = $6.
	priced := &model{slashCtx: &slashCtx{agt: &fakeAgentControl{
		cumIn:  1_000_000,
		cumOut: 500_000,
		prov:   &llm.Provider{InputPrice: 3, OutputPrice: 6},
	}}}
	if got := ansi.Strip(priced.costIndicator()); got != "$6.00" {
		t.Fatalf("priced session: want $6.00; got %q", got)
	}
	if got := viewText(priced); !strings.Contains(got, "$6.00") {
		t.Fatalf("status line should carry the cost badge; got:\n%s", got)
	}

	// Sub-cent spend keeps enough digits to read as non-zero.
	tiny := &model{slashCtx: &slashCtx{agt: &fakeAgentControl{
		cumIn: 1000, prov: &llm.Provider{InputPrice: 3},
	}}}
	if got := ansi.Strip(tiny.costIndicator()); got != "$0.0030" {
		t.Fatalf("sub-cent session: want $0.0030; got %q", got)
	}

	// Free / local provider (no prices) → cumulative token badge, not $.
	local := &model{slashCtx: &slashCtx{agt: &fakeAgentControl{
		cumIn: 12_000, cumOut: 3_000, prov: &llm.Provider{},
	}}}
	if got := ansi.Strip(local.costIndicator()); got != "Σ 15k" {
		t.Fatalf("free provider: want Σ 15k; got %q", got)
	}

	// Nil provider also falls back to the token badge (no panic).
	noProv := &model{slashCtx: &slashCtx{agt: &fakeAgentControl{cumIn: 500}}}
	if got := ansi.Strip(noProv.costIndicator()); got != "Σ 500" {
		t.Fatalf("nil provider: want Σ 500; got %q", got)
	}

	// Fresh session, nothing billed yet → no badge.
	fresh := &model{slashCtx: &slashCtx{agt: &fakeAgentControl{
		prov: &llm.Provider{InputPrice: 3},
	}}}
	if got := fresh.costIndicator(); got != "" {
		t.Fatalf("fresh session should produce no badge; got %q", got)
	}

	// True attach mode (no agent control) → nil-safe, empty.
	if got := (&model{}).costIndicator(); got != "" {
		t.Fatalf("nil agent control should produce no badge; got %q", got)
	}
}

// TestFmtCost: adaptive precision across magnitudes.
func TestFmtCost(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "$0.0000"},
		{0.0003, "$0.0003"},
		{0.0156, "$0.016"},
		{0.5, "$0.500"},
		{1.5, "$1.50"},
		{42.125, "$42.12"},
		{250, "$250"},
	}
	for _, c := range cases {
		if got := fmtCost(c.in); got != c.want {
			t.Errorf("fmtCost(%v) = %q; want %q", c.in, got, c.want)
		}
	}
}
