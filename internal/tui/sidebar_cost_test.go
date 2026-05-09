// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/lsp"
	"github.com/TaraTheStar/enso/internal/mcp"
)

func newSidebarWithStub(t *testing.T, stub *stubSidebarAgent) (*Sidebar, *tview.TextView) {
	t.Helper()
	view := tview.NewTextView()
	sb := NewSidebar(
		view,
		stub,
		"abc",
		"/tmp",
		time.Now(),
		lsp.NewManager("/tmp", nil),
		mcp.NewManager(),
	)
	return sb, view
}

func TestSidebar_CumulativeTokenLineHiddenWhenZero(t *testing.T) {
	stub := &stubSidebarAgent{
		provider: &llm.Provider{Name: "local", Model: "stub"},
		window:   10000,
	}
	sb, view := newSidebarWithStub(t, stub)
	sb.Refresh()
	out := view.GetText(true)
	// The cumulative line uses " in · " as a unique marker. Other
	// session-section lines don't.
	if strings.Contains(out, " in · ") {
		t.Errorf("cum line should be hidden with no spend yet: %q", out)
	}
}

func TestSidebar_CumulativeTokenLineRendersTotals(t *testing.T) {
	stub := &stubSidebarAgent{
		provider: &llm.Provider{Name: "local", Model: "stub"},
		window:   10000,
		cumIn:    12_000,
		cumOut:   34_000,
	}
	sb, view := newSidebarWithStub(t, stub)
	sb.Refresh()
	out := view.GetText(true)
	if !strings.Contains(out, "in") || !strings.Contains(out, "out") {
		t.Errorf("expected 'in' and 'out' labels: %q", out)
	}
	if !strings.Contains(out, "total") {
		t.Errorf("expected 'total' segment: %q", out)
	}
}

func TestSidebar_CostHiddenForFreeProvider(t *testing.T) {
	// Local-only providers (llama.cpp, ollama) leave the price
	// fields zero. No cost segment, even with cumulative tokens.
	stub := &stubSidebarAgent{
		provider: &llm.Provider{Name: "local", Model: "stub"},
		window:   10000,
		cumIn:    50_000,
		cumOut:   25_000,
	}
	sb, view := newSidebarWithStub(t, stub)
	sb.Refresh()
	out := view.GetText(true)
	if strings.Contains(out, "$") {
		t.Errorf("cost segment should hide when provider has no pricing: %q", out)
	}
}

func TestSidebar_CostShownForPaidProvider(t *testing.T) {
	stub := &stubSidebarAgent{
		provider: &llm.Provider{
			Name:        "openai",
			Model:       "gpt-4o",
			InputPrice:  2.50,  // $/1M
			OutputPrice: 10.00, // $/1M
		},
		window: 10000,
		cumIn:  10_000,
		cumOut: 5_000,
	}
	sb, view := newSidebarWithStub(t, stub)
	sb.Refresh()
	out := view.GetText(true)
	if !strings.Contains(out, "$") {
		t.Errorf("cost segment should appear for paid provider: %q", out)
	}
}
