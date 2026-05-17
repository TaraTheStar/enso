// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"fmt"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/tools"
)

// overlayData is the snapshot the alt-screen session-inspector overlay
// reads from. The overlay isn't a sidebar (it's a full-screen alt-
// screen takeover, not a side panel); naming reflects that.
//
// Caller (model) holds this in a stable field so the overlay renders
// consistent state even if the underlying agent / managers continue
// mutating in the background.
type overlayData struct {
	agt      agentControl
	cfg      *config.Config
	checker  *permissions.Checker
	registry *tools.Registry
	writer   *session.Writer
	cwd      string
}

// renderOverlay produces the full-screen alt-screen view: a structured
// section list of session state. width/height come from
// tea.WindowSizeMsg; the current single-column layout doesn't use
// them but the signature is ready for fitted layout work.
func renderOverlay(d *overlayData, width, height int) string {
	_ = width
	_ = height
	if d == nil || d.agt == nil {
		return ""
	}

	title := lipgloss.NewStyle().
		Foreground(paletteHex("lavender")).
		Bold(true).
		Render("◯ enso · session inspector")

	var sections []string
	sections = append(sections, sectionModel(d))
	sections = append(sections, sectionSession(d))
	sections = append(sections, sectionRuntime(d))
	sections = append(sections, sectionPermissions(d))

	body := strings.Join(sections, "\n\n")

	footer := lipgloss.NewStyle().
		Foreground(paletteHex("comment")).
		Faint(true).
		Render("Esc / Ctrl-Space  close")

	return lipgloss.JoinVertical(lipgloss.Left,
		title,
		"",
		body,
		"",
		footer,
	)
}

func sectionHeader(name string) string {
	return lipgloss.NewStyle().
		Foreground(paletteHex("comment")).
		Bold(true).
		Render(strings.ToUpper(name))
}

func keyValue(key, value string) string {
	keyStyle := lipgloss.NewStyle().Foreground(paletteHex("comment"))
	return keyStyle.Render(fmt.Sprintf("%-12s", key)) + value
}

func sectionModel(d *overlayData) string {
	p := d.agt.Provider()
	if p == nil {
		// Defensive: behind the seam Provider() resolves the active
		// name against the real provider set; a transient mismatch
		// (before the first telemetry snapshot) yields nil rather than
		// panicking the inspector.
		return strings.Join([]string{sectionHeader("model"), keyValue("name", "(initialising…)")}, "\n")
	}
	used := d.agt.EstimateTokens()
	var lines []string
	lines = append(lines, sectionHeader("model"))
	lines = append(lines, keyValue("name", p.Model))
	lines = append(lines, keyValue("provider", p.Name))
	lines = append(lines, keyValue("context", fmt.Sprintf("%s window · %s used (%s)",
		formatWindow(p.ContextWindow),
		formatWindow(used),
		percentOf(used, p.ContextWindow),
	)))
	lines = append(lines, keyValue("cumulative", fmt.Sprintf("%s in · %s out",
		formatWindow(int(d.agt.CumulativeInputTokens())),
		formatWindow(int(d.agt.CumulativeOutputTokens())),
	)))
	return strings.Join(lines, "\n")
}

func sectionSession(d *overlayData) string {
	var lines []string
	lines = append(lines, sectionHeader("session"))
	if d.writer != nil {
		lines = append(lines, keyValue("id", d.writer.SessionID()))
	} else {
		lines = append(lines, keyValue("id", "ephemeral"))
	}
	lines = append(lines, keyValue("cwd", d.cwd))
	return strings.Join(lines, "\n")
}

func sectionRuntime(d *overlayData) string {
	var lines []string
	lines = append(lines, sectionHeader("runtime"))

	if n := len(d.cfg.MCP); n > 0 {
		names := make([]string, 0, n)
		for name := range d.cfg.MCP {
			names = append(names, name)
		}
		sort.Strings(names)
		lines = append(lines, keyValue("mcp", strings.Join(names, ", ")))
	} else {
		lines = append(lines, keyValue("mcp", lipgloss.NewStyle().Foreground(paletteHex("comment")).Render("(none)")))
	}

	if n := len(d.cfg.LSP); n > 0 {
		names := make([]string, 0, n)
		for name := range d.cfg.LSP {
			names = append(names, name)
		}
		sort.Strings(names)
		lines = append(lines, keyValue("lsp", strings.Join(names, ", ")))
	} else {
		lines = append(lines, keyValue("lsp", lipgloss.NewStyle().Foreground(paletteHex("comment")).Render("(none)")))
	}

	toolsList := d.registry.List()
	lines = append(lines, keyValue("tools", fmt.Sprintf("%d registered", len(toolsList))))
	return strings.Join(lines, "\n")
}

func sectionPermissions(d *overlayData) string {
	var lines []string
	lines = append(lines, sectionHeader("permissions"))
	mode := "PROMPT"
	if d.checker.Yolo() {
		mode = "AUTO (yolo)"
	}
	lines = append(lines, keyValue("mode", mode))
	return strings.Join(lines, "\n")
}

// Compile-time guards keeping the import set explicit even if a
// future refactor drops a particular call. Each pinned package has
// at least one direct reference elsewhere in this file.
var (
	_ = (*tools.Registry)(nil)
	_ = (*session.Writer)(nil)
	_ = (*permissions.Checker)(nil)
	_ = (*config.Config)(nil)
)
