// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"strings"
	"testing"

	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/bus"
)

func TestIsUntrustedContentTool(t *testing.T) {
	cases := map[string]bool{
		// AGENTS.md-flagged tools must all match.
		"bash":            true,
		"read":            true,
		"web_fetch":       true,
		"lsp_hover":       true,
		"lsp_definition":  true,
		"lsp_references":  true,
		"lsp_diagnostics": true,
		// Filesystem-content tools added per design discussion.
		"grep": true,
		"glob": true,
		// User-supplied or internal-state tools must NOT match.
		"write":       false,
		"edit":        false,
		"web_search":  false,
		"todo":        false,
		"memory_save": false,
		// Unknown / future / MCP tools must default to false. MCP
		// servers carry their own threat profile and need an
		// explicit decision before getting the marker.
		"":                       false,
		"mcp__some__tool":        false,
		"unknown_tool_invented":  false,
	}
	for name, want := range cases {
		if got := isUntrustedContentTool(name); got != want {
			t.Errorf("isUntrustedContentTool(%q)=%v, want %v", name, got, want)
		}
	}
}

func TestToolBlockRender_MarkerForContentBearingTool(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{
		"id":   "t1",
		"name": "bash",
		"args": map[string]any{"cmd": "ls"},
	}})
	got := c.view.GetText(false)
	if !strings.Contains(got, "⚑") {
		t.Errorf("bash call should carry the untrusted marker: %q", got)
	}
}

func TestToolBlockRender_NoMarkerForUserSuppliedTool(t *testing.T) {
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{
		"id":   "t1",
		"name": "write",
		"args": map[string]any{"path": "/tmp/x", "content": "hi"},
	}})
	got := c.view.GetText(false)
	if strings.Contains(got, "⚑") {
		t.Errorf("write should not carry the untrusted marker: %q", got)
	}
}

func TestToolBlockRender_MarkerSurvivesRedraw(t *testing.T) {
	// Full-render path (Ctrl-T toggle, /find, etc.) must produce the
	// same marker as the live-paint path. Regression-guard for the
	// two-site marker logic.
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{
		"id":   "t1",
		"name": "read",
		"args": map[string]any{"path": "/etc/passwd"},
	}})
	c.Redraw()
	got := c.view.GetText(false)
	if !strings.Contains(got, "⚑") {
		t.Errorf("redraw lost the untrusted marker: %q", got)
	}
}

func TestToolBlockRender_UnknownToolNoMarker(t *testing.T) {
	// MCP-style tool name. Until we make an explicit decision about
	// MCP servers' threat profile, they don't get the marker.
	c := NewChatDisplay(tview.NewTextView(), "test")
	c.Render(bus.Event{Type: bus.EventToolCallStart, Payload: map[string]any{
		"id":   "t1",
		"name": "mcp__myserver__some_tool",
		"args": map[string]any{},
	}})
	got := c.view.GetText(false)
	if strings.Contains(got, "⚑") {
		t.Errorf("MCP tool should not get the untrusted marker by default: %q", got)
	}
}
