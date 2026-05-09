// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/lsp"
	"github.com/TaraTheStar/enso/internal/mcp"
)

// SidebarAgent is the subset of *agent.Agent the sidebar needs. Wrapped
// behind an interface so tests can fake it without dragging the agent
// package and its transitive deps into the test build.
type SidebarAgent interface {
	Provider() *llm.Provider
	EstimateTokens() int
	ContextWindow() int
	CumulativeInputTokens() int64
	CumulativeOutputTokens() int64
}

// Sidebar renders the right-hand session inspector. Sections, top to
// bottom: Session (model · tokens bar · duration · cwd), LSPs, MCPs,
// Changes (git working-tree diff, only when populated), Agents (only
// when populated).
//
// Refresh is cheap (string assembly + one TextView write) so the host
// can call it on a 1s ticker plus on every relevant bus event without
// worrying about cost.
type Sidebar struct {
	view *tview.TextView

	agent        SidebarAgent
	sessionID    string
	sessionLabel string // mu-protected; auto-set on first user message, overridable via /rename
	sessionStart time.Time
	cwd          string
	lspMgr       *lsp.Manager
	mcpMgr       *mcp.Manager

	// Subagent state, populated from EventAgentStart / EventAgentEnd.
	// The map is keyed on agent id so re-renders show the current state
	// of each agent rather than a running history.
	mu     sync.Mutex
	agents map[string]*sidebarAgent
	order  []string // insertion order so the rendered list is stable

	// Git working-tree changes. The cache is mutated only by the
	// gitWorker goroutine; readers (Refresh) take the mutex. nilSentinel
	// distinguishes "not yet fetched" from "fetched, nothing to show":
	// we render nothing in either case, but only the second one means
	// the section reliably stays out of the way when there are no changes.
	gitMu       sync.RWMutex
	gitChanges  []gitChange
	gitFetched  bool          // true after the first fetch completes (success or empty)
	gitTrigger  chan struct{} // size 1; non-blocking sends coalesce bursts
	gitOnUpdate func()        // optional repaint callback, set by SetGitRefreshCallback
}

type sidebarAgent struct {
	id     string
	prompt string
	state  string // running / done / error
	errMsg string
}

// NewSidebar wires a TextView to the host's session state. start is the
// wall-clock time the session began, used for the "Active for …" line.
func NewSidebar(
	view *tview.TextView,
	a SidebarAgent,
	sessionID, cwd string,
	start time.Time,
	lspMgr *lsp.Manager,
	mcpMgr *mcp.Manager,
) *Sidebar {
	view.SetDynamicColors(true)
	view.SetWrap(true)
	return &Sidebar{
		view:         view,
		agent:        a,
		sessionID:    sessionID,
		sessionStart: start,
		cwd:          cwd,
		lspMgr:       lspMgr,
		mcpMgr:       mcpMgr,
		agents:       make(map[string]*sidebarAgent),
		gitTrigger:   make(chan struct{}, 1),
	}
}

// SetLabel updates the session's display label. Caller is responsible
// for triggering Refresh afterwards (typically batched with other
// state updates on the tview goroutine).
func (s *Sidebar) SetLabel(label string) {
	s.mu.Lock()
	s.sessionLabel = label
	s.mu.Unlock()
}

// SetGitRefreshCallback registers a function the git-status worker calls
// after it updates the cache. The host wires this to repaint the sidebar
// on the tview goroutine — the worker itself runs in its own goroutine
// and shouldn't touch tview directly.
func (s *Sidebar) SetGitRefreshCallback(f func()) { s.gitOnUpdate = f }

// TriggerGitRefresh schedules an async git-status fetch. Safe to call
// from any goroutine and from any frequency — bursts (e.g. a burst of
// edit/write tool calls in one turn) collapse into a single fetch via
// the buffered channel.
func (s *Sidebar) TriggerGitRefresh() {
	if s.gitTrigger == nil {
		return
	}
	select {
	case s.gitTrigger <- struct{}{}:
	default:
		// A fetch is already queued; nothing to do.
	}
}

// RunGitWatcher is the worker that consumes triggers, debounces them,
// runs the git call, and notifies the host via gitOnUpdate. Blocks until
// ctx is cancelled. Run in its own goroutine.
//
// Debounce: when a trigger arrives we wait `debounce` ms for any further
// triggers before running git. A turn that writes ten files thus pays
// one git invocation, not ten.
func (s *Sidebar) RunGitWatcher(ctx context.Context) {
	const debounce = 250 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.gitTrigger:
		}
		// Drain any further triggers that arrived during the debounce
		// window so a burst still results in one fetch.
	debounceLoop:
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(debounce):
				break debounceLoop
			case <-s.gitTrigger:
				// reset the timer
			}
		}
		changes := fetchGitChanges(s.cwd)
		s.gitMu.Lock()
		s.gitChanges = changes
		s.gitFetched = true
		s.gitMu.Unlock()
		if s.gitOnUpdate != nil {
			s.gitOnUpdate()
		}
	}
}

// HandleEvent updates the agent-section state from a bus event. Caller
// is responsible for calling Refresh afterwards (typically batched into
// the same QueueUpdateDraw block as the chat redraw).
func (s *Sidebar) HandleEvent(ev bus.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch ev.Type {
	case bus.EventAgentStart:
		m, _ := ev.Payload.(map[string]any)
		if m == nil {
			return
		}
		id, _ := m["id"].(string)
		if id == "" {
			return
		}
		prompt, _ := m["prompt"].(string)
		if _, exists := s.agents[id]; !exists {
			s.order = append(s.order, id)
		}
		s.agents[id] = &sidebarAgent{id: id, prompt: prompt, state: "running"}
	case bus.EventAgentEnd:
		m, _ := ev.Payload.(map[string]any)
		if m == nil {
			return
		}
		id, _ := m["id"].(string)
		if id == "" {
			return
		}
		errStr, _ := m["error"].(string)
		entry, ok := s.agents[id]
		if !ok {
			entry = &sidebarAgent{id: id}
			s.agents[id] = entry
			s.order = append(s.order, id)
		}
		if errStr != "" {
			entry.state = "error"
			entry.errMsg = errStr
		} else {
			entry.state = "done"
		}
	}
}

// Refresh re-renders the entire sidebar from the current state. Cheap
// enough to call from a 1s ticker — the TextView diff is one SetText
// call and tview only redraws when the screen is dirty.
func (s *Sidebar) Refresh() {
	var sb strings.Builder
	s.renderSession(&sb)
	sb.WriteString("\n")
	s.renderLSPs(&sb)
	sb.WriteString("\n")
	s.renderMCPs(&sb)

	s.gitMu.RLock()
	hasChanges := len(s.gitChanges) > 0
	s.gitMu.RUnlock()
	if hasChanges {
		sb.WriteString("\n")
		s.renderChanges(&sb)
	}

	s.mu.Lock()
	hasAgents := len(s.agents) > 0
	s.mu.Unlock()
	if hasAgents {
		sb.WriteString("\n")
		s.renderAgents(&sb)
	}

	s.view.SetText(sb.String())
}

// section writes a small dim section header.
func section(sb *strings.Builder, title string) {
	fmt.Fprintf(sb, "[lavender::b]%s[-:-:-]\n", title)
}

func (s *Sidebar) renderSession(sb *strings.Builder) {
	section(sb, "session")
	s.mu.Lock()
	label := s.sessionLabel
	s.mu.Unlock()
	if label != "" {
		fmt.Fprintf(sb, "[lavender]%s[-]\n", truncateOneLine(label, sidebarBarWidth))
	}
	if s.agent != nil {
		p := s.agent.Provider()
		if p != nil {
			fmt.Fprintf(sb, "[gray]%s[-]\n", p.Model)
			fmt.Fprintf(sb, "[comment]via %s[-]\n", p.Name)
		}
		used := s.agent.EstimateTokens()
		window := s.agent.ContextWindow()
		fmt.Fprintf(sb, "%s\n", tokenBar(used, window, sidebarBarWidth))

		// Cumulative-spend line: separate from the context-window
		// bar above (which shrinks after compaction). Tokens count
		// what was actually sent + received over the session, and
		// the optional cost segment uses provider pricing if set.
		cumIn := s.agent.CumulativeInputTokens()
		cumOut := s.agent.CumulativeOutputTokens()
		if cumIn > 0 || cumOut > 0 {
			line := fmt.Sprintf("[comment]%s in · %s out · total %s[-]",
				compactTokenCount(int(cumIn)),
				compactTokenCount(int(cumOut)),
				compactTokenCount(int(cumIn+cumOut)))
			if p != nil && (p.InputPrice > 0 || p.OutputPrice > 0) {
				cost := computeCost(cumIn, cumOut, p.InputPrice, p.OutputPrice)
				line += fmt.Sprintf(" [comment]· %s[-]", formatCost(cost))
			}
			fmt.Fprintln(sb, line)
		}
	}
	if s.sessionID != "" {
		fmt.Fprintf(sb, "[comment]id %s[-]\n", shortID(s.sessionID))
	}
	if !s.sessionStart.IsZero() {
		fmt.Fprintf(sb, "[comment]active %s[-]\n", durationCompact(time.Since(s.sessionStart)))
	}
	if s.cwd != "" {
		fmt.Fprintf(sb, "[comment]%s[-]\n", contractHome(s.cwd))
	}
}

func (s *Sidebar) renderLSPs(sb *strings.Builder) {
	section(sb, "lsps")
	names := s.lspMgr.ConfiguredNames()
	if len(names) == 0 {
		fmt.Fprintf(sb, "[comment](none configured)[-]\n")
		return
	}
	for _, n := range names {
		dot, color := "○", "comment" // idle / not yet spawned
		if s.lspMgr.IsRunning(n) {
			dot, color = "●", "sage"
		}
		fmt.Fprintf(sb, "[%s]%s[-] %s\n", color, dot, n)
	}
}

func (s *Sidebar) renderMCPs(sb *strings.Builder) {
	section(sb, "mcps")
	if s.mcpMgr == nil {
		fmt.Fprintf(sb, "[comment](none configured)[-]\n")
		return
	}
	names := s.mcpMgr.ConfiguredNames()
	if len(names) == 0 {
		fmt.Fprintf(sb, "[comment](none configured)[-]\n")
		return
	}
	servers := s.mcpMgr.Servers()
	for _, n := range names {
		state, reason := s.mcpMgr.State(n)
		if state == mcp.StateFailed {
			line := fmt.Sprintf("[red]✘[-] %s", n)
			if reason != "" {
				line += fmt.Sprintf(" [comment](%s)[-]", truncateOneLine(reason, sidebarBarWidth-len(n)-4))
			}
			fmt.Fprintln(sb, line)
			continue
		}
		toolCount := 0
		if srv := servers[n]; srv != nil {
			toolCount = len(srv.Tools)
		}
		fmt.Fprintf(sb, "[sage]●[-] %s [comment](%d tool%s)[-]\n",
			n, toolCount, plural(toolCount))
	}
}

// renderChanges shows the working-tree diff vs HEAD: one row per changed
// path, capped to keep the sidebar from blowing past a screen height.
// Section is hidden entirely when the cache is nil (not a git repo) or
// empty (clean tree); the gating happens in Refresh, not here.
func (s *Sidebar) renderChanges(sb *strings.Builder) {
	s.gitMu.RLock()
	changes := s.gitChanges
	s.gitMu.RUnlock()

	section(sb, fmt.Sprintf("changes (%d)", len(changes)))

	const maxRows = 8
	rendered := changes
	overflow := 0
	if len(rendered) > maxRows {
		overflow = len(rendered) - maxRows
		rendered = rendered[:maxRows]
	}
	for _, c := range rendered {
		mark, color := changeGlyph(c.Status)
		path := truncateOneLine(c.Path, sidebarBarWidth-2)
		fmt.Fprintf(sb, "[%s]%s[-] %s\n", color, mark, path)
	}
	if overflow > 0 {
		fmt.Fprintf(sb, "[comment]+%d more[-]\n", overflow)
	}
}

// changeGlyph picks a one-char marker and color for a porcelain status
// pair. The pair is "XY" where X is the index slot and Y the work-tree
// slot; we show whichever is non-space (preferring the work-tree slot
// since that's what the user is actively touching). Untracked is "??".
func changeGlyph(status string) (mark, color string) {
	if len(status) < 2 {
		return "?", "comment"
	}
	if status == "??" {
		return "?", "comment"
	}
	worktree := status[1]
	index := status[0]
	pick := byte(' ')
	if worktree != ' ' {
		pick = worktree
	} else if index != ' ' {
		pick = index
	}
	switch pick {
	case 'M':
		return "M", "teal"
	case 'A':
		return "A", "sage"
	case 'D':
		return "D", "red"
	case 'R':
		return "R", "lavender"
	case 'C':
		return "C", "lavender"
	case 'U':
		return "U", "red" // unmerged
	case 'T':
		return "T", "teal" // type change
	}
	return "·", "comment"
}

func (s *Sidebar) renderAgents(sb *strings.Builder) {
	section(sb, "agents")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		entry := s.agents[id]
		if entry == nil {
			continue
		}
		icon, color := "▶", "lavender"
		switch entry.state {
		case "done":
			icon, color = "✓", "sage"
		case "error":
			icon, color = "✘", "red"
		}
		label := shortID(entry.id)
		if entry.prompt != "" {
			label = truncateOneLine(entry.prompt, sidebarBarWidth)
		}
		fmt.Fprintf(sb, "[%s]%s[-] %s\n", color, icon, label)
		if entry.errMsg != "" {
			fmt.Fprintf(sb, "    [red]%s[-]\n", truncateOneLine(entry.errMsg, sidebarBarWidth-2))
		}
	}
}

// sidebarBarWidth is the visible width of the inner content (sidebar
// pane is 30 cols; subtract padding). Used both for the token bar and
// for truncating prompts/errors so they don't wrap.
const sidebarBarWidth = 28

// tokenBar renders a unicode-block progress bar plus a "12k/32k"
// numeric label. Color shifts to dust at 50% and red at 80% to match
// the status-bar warning thresholds.
func tokenBar(used, window, width int) string {
	if window <= 0 {
		return fmt.Sprintf("[comment]%s[-]", compactTokenCount(used))
	}
	frac := float64(used) / float64(window)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	color := "sage"
	switch {
	case frac >= 0.8:
		color = "red"
	case frac >= 0.5:
		color = "dust"
	}
	filled := int(float64(width) * frac)
	if filled < 0 {
		filled = 0
	}
	if filled > width {
		filled = width
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
	label := fmt.Sprintf("%s/%s", compactTokenCount(used), compactTokenCount(window))
	return fmt.Sprintf("[%s]%s[-] [comment]%s[-]", color, bar, label)
}

// durationCompact renders d as a short "Xh Ym" / "Xm Ys" / "Xs" form.
// Designed to fit in a sidebar column without unit gymnastics.
func durationCompact(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		m := int(d.Minutes())
		s := int(d.Seconds()) - m*60
		return fmt.Sprintf("%dm %ds", m, s)
	default:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		return fmt.Sprintf("%dh %dm", h, m)
	}
}

// contractHome replaces the user's home directory prefix with `~` so a
// long path like /home/alice/go/src/x renders as ~/go/src/x. Falls back
// to showing only the last two path segments if the result still won't
// fit the sidebar column.
func contractHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(p, home) {
		p = "~" + strings.TrimPrefix(p, home)
	}
	if len(p) > sidebarBarWidth {
		segs := strings.Split(p, string(filepath.Separator))
		if len(segs) > 2 {
			p = "…/" + strings.Join(segs[len(segs)-2:], string(filepath.Separator))
		}
	}
	return p
}
