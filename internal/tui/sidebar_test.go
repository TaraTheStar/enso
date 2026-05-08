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

// stubSidebarAgent satisfies SidebarAgent without dragging in the real
// agent package.
type stubSidebarAgent struct {
	provider *llm.Provider
	tokens   int
	window   int
}

func (s *stubSidebarAgent) Provider() *llm.Provider { return s.provider }
func (s *stubSidebarAgent) EstimateTokens() int     { return s.tokens }
func (s *stubSidebarAgent) ContextWindow() int      { return s.window }

func newTestSidebar(t *testing.T, sessionID string) (*Sidebar, *tview.TextView) {
	t.Helper()
	view := tview.NewTextView()
	stub := &stubSidebarAgent{
		provider: &llm.Provider{Name: "test", Model: "stub"},
		tokens:   100,
		window:   10000,
	}
	sb := NewSidebar(
		view,
		stub,
		sessionID,
		"/tmp",
		time.Now(),
		lsp.NewManager("/tmp", nil),
		mcp.NewManager(),
	)
	return sb, view
}

func TestSidebar_LabelHiddenWhenUnset(t *testing.T) {
	sb, view := newTestSidebar(t, "abcdef1234")
	sb.Refresh()
	out := view.GetText(true)
	if strings.Contains(out, "[lavender]") && !strings.Contains(out, "session") {
		t.Errorf("unexpected lavender content with no label set: %q", out)
	}
	// The id line should still render.
	if !strings.Contains(out, "id ") {
		t.Errorf("session id missing from sidebar: %q", out)
	}
}

func TestSidebar_LabelRendersAfterSet(t *testing.T) {
	sb, view := newTestSidebar(t, "abcdef1234")
	sb.SetLabel("fix-the-flaky-auth-test")
	sb.Refresh()
	out := view.GetText(true)
	if !strings.Contains(out, "fix-the-flaky-auth-test") {
		t.Errorf("label not rendered in sidebar: %q", out)
	}
	// Label sits ABOVE the id line so users see the human-readable
	// identity first.
	labelIdx := strings.Index(out, "fix-the-flaky-auth-test")
	idIdx := strings.Index(out, "id ")
	if labelIdx < 0 || idIdx < 0 || labelIdx > idIdx {
		t.Errorf("label should appear before id (label=%d id=%d)\nout: %s", labelIdx, idIdx, out)
	}
}

func TestSidebar_LabelTruncatedToBarWidth(t *testing.T) {
	sb, view := newTestSidebar(t, "abcdef1234")
	long := strings.Repeat("x", sidebarBarWidth+10)
	sb.SetLabel(long)
	sb.Refresh()
	out := view.GetText(true)
	// truncateOneLine inserts an ellipsis when cutting; the raw long
	// string must not survive in full.
	if strings.Contains(out, long) {
		t.Errorf("oversized label not truncated: %q", out)
	}
}
