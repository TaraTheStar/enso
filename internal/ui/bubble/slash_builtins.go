// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TaraTheStar/enso/internal/agents"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/mcp"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/slash"
	"github.com/TaraTheStar/enso/internal/ui/blocks"
	"github.com/TaraTheStar/enso/internal/ui/find"
	"github.com/TaraTheStar/enso/internal/workflow"
)

// registerBuiltins installs the bubble-side slash commands. The set
// here is deliberately a subset of internal/tui's builtins — anything
// requiring tview overlays (file picker, find, grep, permissions
// editor, compact-confirm modal) is omitted until phase 5 ports those
// flows. Long-running commands that need a goroutine + program.Send
// pattern (e.g. /compact) also wait for that infra to land.
func registerBuiltins(reg *slash.Registry, sc *slashCtx) {
	reg.Register(&helpCmd{reg: reg, sc: sc})
	reg.Register(&quitCmd{sc: sc})
	reg.Register(&yoloCmd{sc: sc})
	reg.Register(&toolsCmd{sc: sc})
	reg.Register(&modelCmd{sc: sc})
	reg.Register(&infoCmd{sc: sc})
	reg.Register(&sessionsCmd{sc: sc})
	reg.Register(&agentsCmd{sc: sc})
	reg.Register(&renameCmd{sc: sc})
	reg.Register(&exportCmd{sc: sc})
	reg.Register(&forkCmd{sc: sc})
	reg.Register(&statsCmd{sc: sc})
	reg.Register(&findSlashCmd{sc: sc})
	reg.Register(&grepSlashCmd{sc: sc})
	reg.Register(&permsCmd{sc: sc})
	reg.Register(&compactCmd{sc: sc})
	reg.Register(&initCmd{sc: sc})
	reg.Register(&loopCmd{sc: sc})
	reg.Register(&workflowCmd{sc: sc})
	reg.Register(&lspCmd{sc: sc})
	reg.Register(&mcpCmd{sc: sc})
	reg.Register(&gitCmd{sc: sc})
	reg.Register(&costCmd{sc: sc})
	reg.Register(&transcriptCmd{sc: sc})
	reg.Register(&contextCmd{sc: sc})
	reg.Register(&pruneCmd{sc: sc})
}

// /context — per-category breakdown of the current prompt prefix.
//
// Surfaces what's actually being sent to the model on the next turn,
// split into system / pinned / active-tool / stubbed-tool /
// conversation. Useful for debugging "why is my prompt still 80k after
// /prune" kinds of questions.

type contextCmd struct{ sc *slashCtx }

func (c *contextCmd) Name() string { return "context" }
func (c *contextCmd) Description() string {
	return "show per-category token breakdown of the current prompt prefix"
}
func (c *contextCmd) Run(ctx context.Context, args string) error {
	if c.sc.agt == nil {
		c.sc.printf("context: no agent")
		return nil
	}
	bd := c.sc.agt.PrefixBreakdown()
	window := c.sc.agt.ContextWindow()

	c.sc.printf("Current prompt prefix:")
	c.sc.printf("  system          %s", formatWindow(bd.System))
	c.sc.printf("  pinned content  %s", formatWindow(bd.Pinned))
	c.sc.printf("  tool (active)   %s", formatWindow(bd.ToolActive))
	c.sc.printf("  tool (stubbed)  %s", formatWindow(bd.ToolStubbed))
	c.sc.printf("  conversation    %s", formatWindow(bd.Conversation))
	c.sc.printf("  --")
	if window > 0 {
		c.sc.printf("  total           %s / %s (%s)",
			formatWindow(bd.Total), formatWindow(window), percentOf(bd.Total, window))
	} else {
		c.sc.printf("  total           %s", formatWindow(bd.Total))
	}
	if bd.ToolActive > bd.Conversation*2 && bd.ToolActive > 5000 {
		c.sc.printf("")
		c.sc.printf("note: tool output dominates — /prune will stub older tool results, freeing tokens.")
	}
	return nil
}

// /prune — manual trigger for aggressive stale-tool stubbing.
//
// Replaces every tool message older than the most recent user-turn
// with a short stub (preserving tool_call_id pairing for OpenAI-shape
// API compatibility). Pinned messages are spared.

type pruneCmd struct{ sc *slashCtx }

func (c *pruneCmd) Name() string { return "prune" }
func (c *pruneCmd) Description() string {
	return "drop stale tool-result payloads from history (keeps the most recent user-turn intact)"
}
func (c *pruneCmd) Run(ctx context.Context, args string) error {
	if c.sc.agt == nil {
		c.sc.printf("prune: no agent")
		return nil
	}
	stubbed, before, after := c.sc.agt.ForcePrune()
	if stubbed == 0 {
		c.sc.printf("prune: nothing to prune (history at %s tokens)", formatWindow(before))
		return nil
	}
	saved := before - after
	c.sc.printf("prune: stubbed %d tool message%s · %s → %s tokens (-%s)",
		stubbed, plural(stubbed),
		formatWindow(before), formatWindow(after), formatWindow(saved))
	return nil
}

// /help

type helpCmd struct {
	reg *slash.Registry
	sc  *slashCtx
}

func (c *helpCmd) Name() string        { return "help" }
func (c *helpCmd) Description() string { return "list available slash commands" }
func (c *helpCmd) Run(ctx context.Context, args string) error {
	c.sc.printf("Slash commands:")
	for _, cm := range c.reg.List() {
		c.sc.printf("  /%-10s  %s", cm.Name(), cm.Description())
	}
	return nil
}

// /quit

type quitCmd struct{ sc *slashCtx }

func (c *quitCmd) Name() string        { return "quit" }
func (c *quitCmd) Description() string { return "exit enso (same as Ctrl-D)" }
func (c *quitCmd) Run(ctx context.Context, args string) error {
	c.sc.quit = true
	return nil
}

// /yolo

type yoloCmd struct{ sc *slashCtx }

func (c *yoloCmd) Name() string        { return "yolo" }
func (c *yoloCmd) Description() string { return "toggle auto-allow mode (on|off)" }
func (c *yoloCmd) Run(ctx context.Context, args string) error {
	enable := false
	switch strings.TrimSpace(strings.ToLower(args)) {
	case "":
		enable = !c.sc.checker.Yolo()
	case "on", "true", "1":
		enable = true
	case "off", "false", "0":
		enable = false
	default:
		c.sc.printf("yolo: usage /yolo [on|off]  (no arg toggles)")
		return nil
	}
	c.sc.checker.SetYolo(enable)
	if enable {
		c.sc.printf("yolo: on (all tool calls auto-allowed)")
	} else {
		// Bubble's permission flow lands in phase 5; until then
		// non-yolo will block tool calls entirely. Surface that.
		c.sc.printf("yolo: off — note: bubble's permission prompt arrives in phase 5; tool calls will fail until then")
	}
	return nil
}

// /tools

type toolsCmd struct{ sc *slashCtx }

func (c *toolsCmd) Name() string        { return "tools" }
func (c *toolsCmd) Description() string { return "list registered tools" }
func (c *toolsCmd) Run(ctx context.Context, args string) error {
	ts := c.sc.registry.List()
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Name())
	}
	sort.Strings(names)
	c.sc.printf("Tools (%d):", len(names))
	for _, n := range names {
		c.sc.printf("  %s", n)
	}
	return nil
}

// /model

type modelCmd struct{ sc *slashCtx }

func (c *modelCmd) Name() string { return "model" }
func (c *modelCmd) Description() string {
	return "switch the active provider: /model (lists) | /model <name>"
}
func (c *modelCmd) Run(ctx context.Context, args string) error {
	args = strings.TrimSpace(args)
	cur := c.sc.agt.Provider()
	if cur == nil {
		c.sc.printf("model: no active provider")
		return nil
	}
	providers := c.sc.providers
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	if args == "" {
		c.sc.printf("Configured providers (current marked *):")
		for _, n := range names {
			marker := "  "
			if n == cur.Name {
				marker = " *"
			}
			p := providers[n]
			c.sc.printf("%s %-20s  %s  ctx=%s", marker, n, p.Model, formatWindow(p.ContextWindow))
		}
		c.sc.printf("")
		c.sc.printf("Usage: /model <name>")
		return nil
	}

	target, ok := providers[args]
	if !ok {
		c.sc.printf("model: unknown provider %q (configured: %v)", args, names)
		return nil
	}
	if target.Name == cur.Name {
		c.sc.printf("model: already on %q", args)
		return nil
	}
	if err := c.sc.agt.SetProvider(args); err != nil {
		c.sc.printf("model: %v", err)
		return nil
	}
	c.sc.printf("model: switched to %q (%s, ctx %s)", target.Name, target.Model, formatWindow(target.ContextWindow))

	// Window-asymmetry warning, mirroring tui's /model output.
	used := c.sc.agt.EstimateTokens()
	if target.ContextWindow > 0 && used > 0 {
		thresh := int(float64(target.ContextWindow) * 0.60)
		if used >= target.ContextWindow {
			c.sc.printf("warning: history (%s) exceeds %s window (%s) — next turn will fail. run /compact.",
				formatWindow(used), target.Name, formatWindow(target.ContextWindow))
		} else if used > thresh {
			c.sc.printf("note: history (%s) is past %s's compaction threshold (%s of %s) — /compact recommended.",
				formatWindow(used), target.Name, formatWindow(thresh), formatWindow(target.ContextWindow))
		}
	}
	return nil
}

// /info

type infoCmd struct{ sc *slashCtx }

func (c *infoCmd) Name() string { return "info" }
func (c *infoCmd) Description() string {
	return "print model, provider, session, and runtime state into scrollback"
}
func (c *infoCmd) Run(ctx context.Context, args string) error {
	prov := c.sc.agt.Provider()
	c.sc.printf("model:    %s", prov.Model)
	c.sc.printf("provider: %s", prov.Name)
	c.sc.printf("context:  %s window · %s used (%s)",
		formatWindow(prov.ContextWindow),
		formatWindow(c.sc.agt.EstimateTokens()),
		percentOf(c.sc.agt.EstimateTokens(), prov.ContextWindow))

	if c.sc.writer != nil {
		c.sc.printf("session:  %s", c.sc.writer.SessionID())
	} else {
		c.sc.printf("session:  ephemeral")
	}
	c.sc.printf("cwd:      %s", c.sc.cwd)

	tools := c.sc.registry.List()
	c.sc.printf("tools:    %d registered", len(tools))

	mode := "PROMPT"
	if c.sc.checker.Yolo() {
		mode = "AUTO (yolo)"
	}
	c.sc.printf("mode:     %s", mode)
	return nil
}

// formatWindow renders a token count as a compact "Xk" or "X.YM"
// string. Mirrors internal/tui/slash_builtins.go for output parity.
func formatWindow(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// percentOf returns "N%" or "" when the window is unconfigured.
func percentOf(used, window int) string {
	if window <= 0 {
		return "?"
	}
	return fmt.Sprintf("%d%%", used*100/window)
}

// /sessions

type sessionsCmd struct{ sc *slashCtx }

func (c *sessionsCmd) Name() string { return "sessions" }
func (c *sessionsCmd) Description() string {
	return "list recent sessions (resume by re-running with --session <id>)"
}
func (c *sessionsCmd) Run(ctx context.Context, args string) error {
	if c.sc.store == nil {
		c.sc.printf("sessions: store unavailable (running --ephemeral)")
		return nil
	}
	infos, err := session.ListRecent(c.sc.store, 20)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	if len(infos) == 0 {
		c.sc.printf("no sessions yet")
		return nil
	}
	c.sc.printf("Recent sessions:")
	for _, info := range infos {
		flag := ""
		if info.Interrupted {
			flag = " [interrupted]"
		}
		c.sc.printf("  %s  %s  %s%s",
			info.ID, info.UpdatedAt.Format("2006-01-02 15:04"), info.Cwd, flag)
	}
	c.sc.printf("")
	c.sc.printf("Resume: enso --session <id>")
	return nil
}

// /agents

type agentsCmd struct{ sc *slashCtx }

func (c *agentsCmd) Name() string { return "agents" }
func (c *agentsCmd) Description() string {
	return "list available declarative agents (built-in, ~/.enso/agents/, ./.enso/agents/)"
}
func (c *agentsCmd) Run(ctx context.Context, args string) error {
	specs, err := agents.LoadAll(c.sc.cwd)
	if err != nil {
		c.sc.printf("agents: %v", err)
		return nil
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })

	c.sc.printf("Available agents:")
	c.sc.printf("  %-12s  full default tool access", "default")
	for _, s := range specs {
		desc := s.Description
		if desc == "" {
			desc = "(no description)"
		}
		c.sc.printf("  %-12s  %s", s.Name, desc)
	}
	c.sc.printf("")
	c.sc.printf("Switch by re-launching with --agent <name> (mid-session switch is not yet supported).")
	return nil
}

// /rename

type renameCmd struct{ sc *slashCtx }

func (c *renameCmd) Name() string { return "rename" }
func (c *renameCmd) Description() string {
	return "show or override the session's display label: /rename | /rename <label>"
}
func (c *renameCmd) Run(ctx context.Context, args string) error {
	args = strings.TrimSpace(args)
	if c.sc.writer == nil {
		c.sc.printf("rename: unavailable (running --ephemeral)")
		return nil
	}
	if args == "" {
		current, err := c.sc.writer.Label()
		if err != nil {
			return fmt.Errorf("read label: %w", err)
		}
		if current == "" {
			c.sc.printf("rename: no label set yet — usage /rename <label>")
		} else {
			c.sc.printf("rename: current label %q (override with /rename <label>)", current)
		}
		return nil
	}
	slug := session.SlugifyLabel(args)
	if slug == "" {
		c.sc.printf("rename: %q has no usable characters (alphanumerics required)", args)
		return nil
	}
	if err := c.sc.writer.SetLabel(slug); err != nil {
		return fmt.Errorf("set label: %w", err)
	}
	c.sc.printf("rename: label set to %q", slug)
	return nil
}

// /export

type exportCmd struct{ sc *slashCtx }

func (c *exportCmd) Name() string { return "export" }
func (c *exportCmd) Description() string {
	return "save the current session as markdown: /export [path] (default: <cwd>/.enso/exports/)"
}
func (c *exportCmd) Run(ctx context.Context, args string) error {
	if c.sc.store == nil {
		c.sc.printf("export: store unavailable (running --ephemeral)")
		return nil
	}
	if c.sc.writer == nil {
		c.sc.printf("export: no session writer")
		return nil
	}
	sessionID := c.sc.writer.SessionID()
	if sessionID == "" {
		c.sc.printf("export: no session id")
		return nil
	}
	path := strings.TrimSpace(args)
	if path == "" {
		short := sessionID
		if len(short) > 8 {
			short = short[:8]
		}
		dir := filepath.Join(c.sc.cwd, ".enso", "exports")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("mkdir: %w", err)
		}
		path = filepath.Join(dir, short+".md")
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	if err := session.WriteMarkdownByID(f, c.sc.store, sessionID); err != nil {
		return fmt.Errorf("write markdown: %w", err)
	}
	c.sc.printf("export: wrote %s", path)
	return nil
}

// /fork

type forkCmd struct{ sc *slashCtx }

func (c *forkCmd) Name() string { return "fork" }
func (c *forkCmd) Description() string {
	return "create a new session copy of this one (prints the new id)"
}
func (c *forkCmd) Run(ctx context.Context, args string) error {
	if c.sc.store == nil {
		c.sc.printf("fork: store unavailable (running --ephemeral)")
		return nil
	}
	if c.sc.writer == nil {
		c.sc.printf("fork: no session writer")
		return nil
	}
	newID, err := session.Fork(c.sc.store, c.sc.writer.SessionID())
	if err != nil {
		return fmt.Errorf("fork: %w", err)
	}
	c.sc.printf("fork: new session %s — resume with `enso --session %s`", newID, newID)
	return nil
}

// /stats

type statsCmd struct{ sc *slashCtx }

func (c *statsCmd) Name() string { return "stats" }
func (c *statsCmd) Description() string {
	return "session activity summary: /stats [--all]"
}
func (c *statsCmd) Run(ctx context.Context, args string) error {
	args = strings.TrimSpace(args)
	if args == "--all" {
		if c.sc.store == nil {
			c.sc.printf("stats: store unavailable (running --ephemeral)")
			return nil
		}
		st, err := session.ComputeStats(c.sc.store, time.Time{})
		if err != nil {
			return fmt.Errorf("stats: %w", err)
		}
		var buf strings.Builder
		if err := session.WriteStatsText(&buf, st, time.Time{}); err != nil {
			return fmt.Errorf("render: %w", err)
		}
		// Strip trailing newline so the dispatcher's wrapper doesn't
		// add a blank line after the summary.
		c.sc.out.WriteString(strings.TrimRight(buf.String(), "\n"))
		c.sc.out.WriteByte('\n')
		return nil
	}
	if args != "" {
		c.sc.printf("stats: usage /stats [--all]")
		return nil
	}
	// Live agent stats: token counts. Lighter touch than the --all DB query.
	if c.sc.agt == nil {
		c.sc.printf("stats: no agent")
		return nil
	}
	c.sc.printf("Current session:")
	c.sc.printf("  estimate:    %s tokens", formatWindow(c.sc.agt.EstimateTokens()))
	c.sc.printf("  cumulative:  %s in · %s out",
		formatWindow(int(c.sc.agt.CumulativeInputTokens())),
		formatWindow(int(c.sc.agt.CumulativeOutputTokens())),
	)
	return nil
}

// /find — substring/regex search over this session's history.
//
// Inline output rather than an alt-screen overlay: in scrollback-native
// the chat is in the terminal's own scrollback, and any "jump to
// match" UX collapses to "scroll up yourself." Printing the matches
// inline gives the same information density without losing the
// terminal-native selection win that motivated the rewrite.

type findSlashCmd struct{ sc *slashCtx }

func (c *findSlashCmd) Name() string { return "find" }
func (c *findSlashCmd) Description() string {
	return "search this session: /find [-e] <pattern>  (-e for regex)"
}
func (c *findSlashCmd) Run(ctx context.Context, args string) error {
	if c.sc.conv == nil {
		c.sc.printf("find: conversation unavailable")
		return nil
	}
	useRegex := false
	pattern := strings.TrimSpace(args)
	if strings.HasPrefix(pattern, "-e ") || pattern == "-e" {
		useRegex = true
		pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "-e"))
	}
	if pattern == "" {
		c.sc.printf("find: usage /find [-e] <pattern>")
		return nil
	}
	sources, idx := blocksToFindSources(c.sc.conv.History())
	hits, err := find.Search(sources, pattern, useRegex)
	if err != nil {
		return fmt.Errorf("find: %w", err)
	}
	if len(hits) == 0 {
		c.sc.printf("find: no matches for %q in this session", pattern)
		return nil
	}
	c.sc.printf("find: %d match%s for %q", len(hits), plural(len(hits)), pattern)
	for _, h := range hits {
		snippet := buildFindSnippet(sources[h.SourceIdx].Text, h.Start, h.End)
		c.sc.printf("  [%-9s] %s", abbreviate(h.Role, 9), snippet)
		_ = idx
	}
	return nil
}

// /grep — search past sessions in the local store.

type grepSlashCmd struct{ sc *slashCtx }

func (c *grepSlashCmd) Name() string { return "grep" }
func (c *grepSlashCmd) Description() string {
	return "search past sessions: /grep [--all] [--regex] <pattern>"
}
func (c *grepSlashCmd) Run(ctx context.Context, args string) error {
	if c.sc.store == nil {
		c.sc.printf("grep: store unavailable (running --ephemeral)")
		return nil
	}
	all, useRegex := false, false
	pattern := strings.TrimSpace(args)
	for {
		switch {
		case strings.HasPrefix(pattern, "--all"):
			all = true
			pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "--all"))
		case strings.HasPrefix(pattern, "--regex"):
			useRegex = true
			pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "--regex"))
		default:
			goto flagsDone
		}
	}
flagsDone:
	if pattern == "" {
		c.sc.printf("grep: usage /grep [--all] [--regex] <pattern>")
		return nil
	}
	const maxHits = 30
	scope := c.sc.cwd
	if all {
		scope = ""
	}
	var hits []session.Hit
	var err error
	if useRegex {
		re, reErr := regexp.Compile(pattern)
		if reErr != nil {
			c.sc.printf("grep: invalid regex: %v", reErr)
			return nil
		}
		hits, err = session.SearchRegex(c.sc.store, re, scope, maxHits+1)
	} else {
		hits, err = session.Search(c.sc.store, pattern, scope, maxHits+1)
	}
	if err != nil {
		return fmt.Errorf("grep: %w", err)
	}
	if len(hits) == 0 {
		where := "in this cwd"
		if all {
			where = "across any session"
		}
		c.sc.printf("grep: no matches for %q %s", pattern, where)
		return nil
	}
	more := ""
	if len(hits) > maxHits {
		more = fmt.Sprintf("  (more truncated; %d capped)", maxHits)
		hits = hits[:maxHits]
	}
	c.sc.printf("grep: %d match%s for %q%s", len(hits), plural(len(hits)), pattern, more)
	for _, h := range hits {
		c.sc.printf("  %s  %s  %s", shortID(h.SessionID), abbreviate(h.Role, 9), strings.ReplaceAll(h.Snippet, "\n", " "))
	}
	c.sc.printf("")
	c.sc.printf("Resume any: enso --session <id>")
	return nil
}

// /permissions — list permission rules and (with `remove <pattern>`) drop one.
// The full editor overlay tui has stays a future polish; the inline
// list-and-remove flow covers daily use.

type permsCmd struct{ sc *slashCtx }

func (c *permsCmd) Name() string { return "permissions" }
func (c *permsCmd) Description() string {
	return "list permission rules: /permissions | /permissions remove <pattern>"
}
func (c *permsCmd) Run(ctx context.Context, args string) error {
	args = strings.TrimSpace(args)
	if strings.HasPrefix(args, "remove ") {
		pat := strings.TrimSpace(strings.TrimPrefix(args, "remove"))
		if pat == "" {
			c.sc.printf("permissions: usage /permissions remove <pattern>")
			return nil
		}
		// Try removing as ALLOW first, then ASK, then DENY.
		// Persistence happens through config.RemoveRule so the change
		// survives across runs.
		path := config.ProjectLocalPath(c.sc.cwd)
		removed := false
		for _, kind := range []string{"allow", "ask", "deny"} {
			ok, err := config.RemoveRule(path, kind, pat)
			if err != nil {
				return fmt.Errorf("permissions: %w", err)
			}
			if ok {
				removed = true
				c.sc.printf("permissions: removed %s rule %q from %s", kind, pat, path)
				break
			}
		}
		if !removed {
			c.sc.printf("permissions: no rule matched %q", pat)
		}
		return nil
	}
	patterns := c.sc.checker.Patterns()
	if len(patterns) == 0 {
		c.sc.printf("permissions: no rules configured")
		return nil
	}
	c.sc.printf("Permission rules (%d):", len(patterns))
	for _, p := range patterns {
		c.sc.printf("  %-6s  %s(%s)", kindLabel(p.Kind), p.Tool, p.Arg)
	}
	c.sc.printf("")
	c.sc.printf("Remove with: /permissions remove <pattern>")
	return nil
}

// /init — scan the current project and write an orientation doc.
//
// Submits a synthetic user message as if the user typed it. The agent
// runs the full read-only survey + write flow under restricted tools
// (read, grep, glob, write, edit, todo).

type initCmd struct{ sc *slashCtx }

func (c *initCmd) Name() string { return "init" }
func (c *initCmd) Description() string {
	return "scan the current project and write ENSO.md with a quick orientation for future agents"
}
func (c *initCmd) Run(ctx context.Context, args string) error {
	if c.sc.submit == nil {
		c.sc.printf("init: cannot submit (no input hook wired)")
		return nil
	}
	target := strings.TrimSpace(args)
	if target == "" {
		target = "ENSO.md"
	}
	c.sc.printf("init: scanning project; will propose %s", target)
	c.sc.submit(initPromptTemplate(target))
	return nil
}

// initPromptTemplate is the synthetic user message /init injects.
// Mirrors internal/tui's template so behaviour is identical between
// backends. Tool restriction (read/grep/glob/write/edit/todo) is not
// applied in bubble's phase-5 cut — the slash.Skill submit pattern that
// carries `allowedTools` is wired only in tui today; porting it is part
// of phase 5's skill-loading work, deferred for now. The agent's
// default tool set runs the survey just fine; the restriction is a
// defence-in-depth measure rather than a correctness requirement.
func initPromptTemplate(target string) string {
	return fmt.Sprintf(`Please orient yourself in this project and write %s at the repository root.

Steps:
1. Use read / grep / glob to survey the codebase. At minimum identify:
   - The primary language(s) and major frameworks.
   - The high-level directory layout and what each top-level directory is for.
   - How to build, test, and run (Makefile targets, package.json scripts, etc.).
   - Any existing convention docs (CONTRIBUTING.md, AGENTS.md, README.md) so you don't duplicate them.
2. Write %s with the following structure, kept concise (under ~150 lines):
   - One-paragraph project description.
   - "Build / test / run" with the actual commands.
   - "Layout" with one line per top-level directory or package.
   - "Conventions" — only project-specific things that aren't obvious from reading the code (e.g. "no CGO", "use slog not fmt").
   - "Where to be careful" — soak-test risks, fragile areas, anything an agent should look at first when something breaks.

If %s already exists, propose a replacement that preserves anything still accurate and updates the rest. Confirm the diff before writing.`, target, target, target)
}

// /compact — preview + confirm + background context compaction.
//
// Bubble's compaction flow differs from tui's modal: there's no
// confirm dialog. /compact prints the preview and instructs the user
// to confirm with /compact yes. The actual ForceCompact runs in a
// goroutine so the UI doesn't freeze for the LLM summary call;
// EventCompacted (or EventError on failure) flows back through the
// bus and is rendered by the conversation state machine.

type compactCmd struct{ sc *slashCtx }

func (c *compactCmd) Name() string { return "compact" }
func (c *compactCmd) Description() string {
	return "context compaction: /compact (preview) | /compact yes (run)"
}
func (c *compactCmd) Run(ctx context.Context, args string) error {
	args = strings.TrimSpace(strings.ToLower(args))
	preview := c.sc.agt.CompactPreview()
	if preview.NothingToDo {
		c.sc.printf("compact: nothing to summarise")
		return nil
	}
	if args != "yes" && args != "y" {
		c.sc.printf("Compaction preview:")
		c.sc.printf("  before:      ~%s tokens", formatWindow(preview.BeforeTokens))
		c.sc.printf("  estimated:   ~%s tokens after", formatWindow(preview.EstAfterTokens))
		c.sc.printf("  summarising: %d older message%s", preview.MessagesToSummarise, plural(preview.MessagesToSummarise))
		c.sc.printf("")
		c.sc.printf("Confirm: /compact yes")
		return nil
	}
	// Confirmed — run in background. The agent publishes
	// EventCompacted on success; on failure we publish EventError so
	// the user sees the failure inline in scrollback.
	agt := c.sc.agt
	go func() {
		if _, err := agt.ForceCompact(context.Background()); err != nil {
			agt.Bus.Publish(bus.Event{Type: bus.EventError, Payload: fmt.Errorf("compact: %w", err)})
		}
	}()
	c.sc.printf("compact: running… (a Compacted block will appear when complete)")
	return nil
}

// /loop — repeat a synthetic prompt on a fixed interval.
//
// At most one loop is active per session. Re-running with new args
// replaces the existing loop; running with no args (or `off`) stops
// it. The loop pushes the prompt through slashCtx.submit on each
// tick, same path /init uses.

type loopCmd struct {
	sc *slashCtx

	mu     sync.Mutex
	cancel context.CancelFunc
}

func (c *loopCmd) Name() string { return "loop" }
func (c *loopCmd) Description() string {
	return "repeat a prompt on an interval — `/loop <interval> <prompt>`; `/loop off` stops"
}

func (c *loopCmd) Run(ctx context.Context, args string) error {
	args = strings.TrimSpace(args)
	if args == "" || strings.EqualFold(args, "off") {
		if c.stop() {
			c.sc.printf("loop: stopped")
		} else {
			c.sc.printf("loop: no active loop")
		}
		return nil
	}
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		c.sc.printf("loop: usage `/loop <interval> <prompt>` (e.g. `/loop 5m check the deploy`)")
		return nil
	}
	interval, err := time.ParseDuration(parts[0])
	if err != nil {
		c.sc.printf("loop: invalid interval %q (try `5m`, `30s`, `1h`): %v", parts[0], err)
		return nil
	}
	if interval < 5*time.Second {
		c.sc.printf("loop: interval must be at least 5s (got %s)", interval)
		return nil
	}
	prompt := strings.TrimSpace(parts[1])
	if prompt == "" {
		c.sc.printf("loop: prompt is empty")
		return nil
	}
	if c.sc.submit == nil {
		c.sc.printf("loop: no input hook wired")
		return nil
	}

	c.stop() // replace any existing loop
	loopCtx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	go c.tick(loopCtx, interval, prompt)

	c.sc.printf("loop: every %s — %q (run /loop off to stop)", interval, truncatePrompt(prompt, 60))
	return nil
}

func (c *loopCmd) stop() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel == nil {
		return false
	}
	c.cancel()
	c.cancel = nil
	return true
}

func (c *loopCmd) tick(ctx context.Context, interval time.Duration, prompt string) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if c.sc.submit != nil {
				c.sc.submit(prompt)
			}
		}
	}
}

func truncatePrompt(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// /workflow — load and run a declarative workflow, or validate one
// without running.

type workflowCmd struct{ sc *slashCtx }

func (c *workflowCmd) Name() string { return "workflow" }
func (c *workflowCmd) Description() string {
	return "run or validate a declarative workflow: /workflow <name> <args> | /workflow validate <name>"
}
func (c *workflowCmd) Run(ctx context.Context, args string) error {
	parts := strings.SplitN(args, " ", 2)
	if len(parts) == 0 || parts[0] == "" {
		c.sc.printf("workflow: usage /workflow <name> <args>  |  /workflow validate <name>")
		return nil
	}

	if parts[0] == "validate" {
		rest := ""
		if len(parts) == 2 {
			rest = strings.TrimSpace(parts[1])
		}
		if rest == "" {
			c.sc.printf("workflow: usage /workflow validate <name>")
			return nil
		}
		wf, err := workflow.LoadByName(c.sc.cwd, rest)
		if err != nil {
			c.sc.printf("workflow validate: %v", err)
			return nil
		}
		c.sc.printf("workflow %q ok — %d role%s (%v), %d edge%s",
			wf.Name,
			len(wf.RoleOrder), plural(len(wf.RoleOrder)), wf.RoleOrder,
			len(wf.Edges), plural(len(wf.Edges)),
		)
		return nil
	}

	name := parts[0]
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}
	wf, err := workflow.LoadByName(c.sc.cwd, name)
	if err != nil {
		c.sc.printf("workflow: %v", err)
		return nil
	}
	c.sc.printf("workflow %q starting (roles: %v)", wf.Name, wf.RoleOrder)

	// Run synchronously for now. Workflows can take minutes; in a
	// future polish we'd run in a goroutine and surface progress via
	// the bus. For now the user sees the result block when it
	// completes; the dispatcher's wrapper waits.
	res, err := workflow.Run(ctx, wf, rest, c.sc.workflowDeps)
	if err != nil {
		c.sc.printf("workflow %q: %v", wf.Name, err)
		return nil
	}
	for _, role := range wf.RoleOrder {
		out := res.Outputs[role]
		c.sc.printf("%s:", role)
		c.sc.printf("%s", out)
		c.sc.printf("")
	}
	return nil
}

// /lsp — list configured language servers and their running state.

type lspCmd struct{ sc *slashCtx }

func (c *lspCmd) Name() string        { return "lsp" }
func (c *lspCmd) Description() string { return "list configured language servers + state" }
func (c *lspCmd) Run(ctx context.Context, args string) error {
	if c.sc.lspMgr == nil || !c.sc.lspMgr.HasServers() {
		c.sc.printf("lsp: no language servers configured")
		return nil
	}
	names := c.sc.lspMgr.ConfiguredNames()
	c.sc.printf("Language servers (%d):", len(names))
	for _, n := range names {
		state := "idle"
		if c.sc.lspMgr.IsRunning(n) {
			state = "running"
		}
		c.sc.printf("  %-12s  %s", n, state)
	}
	c.sc.printf("")
	c.sc.printf("LSPs start lazily on the first matching tool call (lsp_hover, lsp_definition, etc.).")
	return nil
}

// /mcp — list configured MCP servers, their connection state, and
// the number of tools each contributed.

type mcpCmd struct{ sc *slashCtx }

func (c *mcpCmd) Name() string        { return "mcp" }
func (c *mcpCmd) Description() string { return "list configured MCP servers + state + tool counts" }
func (c *mcpCmd) Run(ctx context.Context, args string) error {
	if c.sc.mcpMgr == nil {
		c.sc.printf("mcp: manager unavailable")
		return nil
	}
	names := c.sc.mcpMgr.ConfiguredNames()
	if len(names) == 0 {
		c.sc.printf("mcp: no servers configured")
		return nil
	}
	servers := c.sc.mcpMgr.Servers()
	c.sc.printf("MCP servers (%d):", len(names))
	for _, n := range names {
		state, errMsg := c.sc.mcpMgr.State(n)
		toolCount := 0
		if srv, ok := servers[n]; ok {
			toolCount = len(srv.Tools)
		}
		switch state {
		case mcp.StateHealthy:
			c.sc.printf("  %-16s  healthy   %d tool%s", n, toolCount, plural(toolCount))
		default:
			detail := errMsg
			if detail == "" {
				detail = "unknown"
			}
			c.sc.printf("  %-16s  %-9s  %s", n, state.String(), detail)
		}
	}
	return nil
}

// /git — branch + working-tree status summary.

type gitCmd struct{ sc *slashCtx }

func (c *gitCmd) Name() string        { return "git" }
func (c *gitCmd) Description() string { return "current branch + working-tree status" }
func (c *gitCmd) Run(ctx context.Context, args string) error {
	branch, err := gitOutput(ctx, c.sc.cwd, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		c.sc.printf("git: %v (not a git repo, or git not installed)", err)
		return nil
	}
	c.sc.printf("branch:  %s", strings.TrimSpace(branch))

	status, err := gitOutput(ctx, c.sc.cwd, "status", "--short")
	if err != nil {
		c.sc.printf("git: status: %v", err)
		return nil
	}
	status = strings.TrimRight(status, "\n")
	if status == "" {
		c.sc.printf("status:  clean")
		return nil
	}
	lines := strings.Split(status, "\n")
	c.sc.printf("status:  %d change%s", len(lines), plural(len(lines)))
	// Cap output so a noisy working tree doesn't fill scrollback.
	const maxShow = 12
	for i, ln := range lines {
		if i >= maxShow {
			c.sc.printf("  …and %d more", len(lines)-maxShow)
			break
		}
		c.sc.printf("  %s", ln)
	}
	return nil
}

// gitOutput runs `git <args...>` in cwd with a short timeout and
// returns stdout (or stderr-as-error).
func gitOutput(ctx context.Context, cwd string, gitArgs ...string) (string, error) {
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(tctx, "git", gitArgs...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("%s", strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(out), nil
}

// /cost — cumulative token totals for the current session.
//
// enso doesn't carry a per-model pricing table; "cost" here is
// token volume, not dollars. If a pricing table lands in config
// later, this command picks up the conversion without callsite
// changes.

type costCmd struct{ sc *slashCtx }

func (c *costCmd) Name() string        { return "cost" }
func (c *costCmd) Description() string { return "cumulative token totals for this session" }
func (c *costCmd) Run(ctx context.Context, args string) error {
	if c.sc.agt == nil {
		c.sc.printf("cost: no agent")
		return nil
	}
	in := c.sc.agt.CumulativeInputTokens()
	out := c.sc.agt.CumulativeOutputTokens()
	c.sc.printf("Cumulative tokens (this session):")
	c.sc.printf("  input:   %s", formatWindow(int(in)))
	c.sc.printf("  output:  %s", formatWindow(int(out)))
	c.sc.printf("  total:   %s", formatWindow(int(in+out)))
	c.sc.printf("")
	c.sc.printf("(no pricing table configured; /stats has the same totals)")
	return nil
}

// /transcript — print a captured subagent transcript into scrollback.
//
// Subagent ids are surfaced via the inline "▸ subagent abc12345
// started" notices the conversation prints when EventAgentStart fires
// with a non-empty parent_id. The user copies the short id and runs
// /transcript abc12345 to see the child's full conversation. With no
// args, /transcript lists every stored id.

type transcriptCmd struct{ sc *slashCtx }

func (c *transcriptCmd) Name() string { return "transcript" }
func (c *transcriptCmd) Description() string {
	return "show a captured subagent transcript: /transcript (lists ids) | /transcript <id-or-prefix>"
}
func (c *transcriptCmd) Run(ctx context.Context, args string) error {
	if c.sc.transcripts == nil {
		c.sc.printf("transcript: no transcripts captured")
		return nil
	}
	ids := c.sc.transcripts.IDs()
	if len(ids) == 0 {
		c.sc.printf("transcript: no subagent transcripts captured yet")
		return nil
	}
	args = strings.TrimSpace(args)
	if args == "" {
		sort.Strings(ids)
		c.sc.printf("Captured subagent transcripts (%d):", len(ids))
		for _, id := range ids {
			short := id
			if len(short) > 12 {
				short = short[:12]
			}
			msgs := c.sc.transcripts.Get(id)
			c.sc.printf("  %s  %d message%s", short, len(msgs), plural(len(msgs)))
		}
		c.sc.printf("")
		c.sc.printf("Show one with: /transcript <id-or-prefix>")
		return nil
	}
	// Resolve by exact id first, then prefix.
	if msgs := c.sc.transcripts.Get(args); msgs != nil {
		c.printMessages(args, msgs)
		return nil
	}
	var hits []string
	for _, id := range ids {
		if strings.HasPrefix(id, args) {
			hits = append(hits, id)
		}
	}
	switch len(hits) {
	case 0:
		c.sc.printf("transcript: no transcript matching %q", args)
	case 1:
		c.printMessages(hits[0], c.sc.transcripts.Get(hits[0]))
	default:
		c.sc.printf("transcript: %q is ambiguous — %d matches:", args, len(hits))
		for _, id := range hits {
			c.sc.printf("  %s", id)
		}
	}
	return nil
}

// printMessages dumps a transcript using the shared block renderer
// so it looks identical to live conversation output. Tool-call and
// reasoning messages from history are skipped — same simplification
// as run.go's replayHistory.
func (c *transcriptCmd) printMessages(id string, msgs []llm.Message) {
	short := id
	if len(short) > 12 {
		short = short[:12]
	}
	c.sc.printf("Transcript %s — %d message%s:", short, len(msgs), plural(len(msgs)))
	c.sc.printf("")
	for _, msg := range msgs {
		var b blocks.Block
		switch msg.Role {
		case "user":
			if msg.Content == "" {
				continue
			}
			b = &blocks.User{Text: msg.Content}
		case "assistant":
			if msg.Content == "" {
				continue
			}
			b = &blocks.Assistant{Text: msg.Content}
		}
		if b == nil {
			continue
		}
		if s := renderBlock(b, 0, true); s != "" {
			c.sc.printf("%s", s)
		}
	}
}

// kindLabel renders permissions.Kind as the same lowercase token the
// config.RemoveRule helper accepts.
func kindLabel(k permissions.Kind) string {
	switch k {
	case permissions.KindAllow:
		return "allow"
	case permissions.KindAsk:
		return "ask"
	case permissions.KindDeny:
		return "deny"
	}
	return "?"
}

// blocksToFindSources flattens conversation history into Source slices
// for find.Search, plus a parallel index back to the original block
// position so /find output can cite the correct turn (currently
// unused — the role label is enough — but kept for future polish).
func blocksToFindSources(history []blocks.Block) ([]find.Source, []int) {
	var sources []find.Source
	var idx []int
	add := func(blockIdx int, role, text string) {
		if text == "" {
			return
		}
		sources = append(sources, find.Source{Role: role, Text: text})
		idx = append(idx, blockIdx)
	}
	for i, b := range history {
		switch v := b.(type) {
		case *blocks.User:
			add(i, "user", v.Text)
		case *blocks.Assistant:
			add(i, "assistant", v.Text)
		case *blocks.Tool:
			add(i, "tool", v.Call)
			if v.Output != "" {
				add(i, "tool-output", v.Output)
			}
		case *blocks.Reasoning:
			add(i, "reasoning", v.Text)
		case *blocks.Error:
			add(i, "error", v.Msg)
		}
	}
	return sources, idx
}

// buildFindSnippet returns a single-line excerpt around a match,
// rendered in plain text (no markup — the caller will pass it through
// the dispatcher's dim wrapper). Newlines are collapsed to spaces.
func buildFindSnippet(text string, start, end int) string {
	const ctx = 24
	from := start - ctx
	if from < 0 {
		from = 0
	}
	to := end + ctx
	if to > len(text) {
		to = len(text)
	}
	prefix := strings.ReplaceAll(text[from:start], "\n", " ")
	match := strings.ReplaceAll(text[start:end], "\n", " ")
	suffix := strings.ReplaceAll(text[end:to], "\n", " ")
	var sb strings.Builder
	if from > 0 {
		sb.WriteString("…")
	}
	sb.WriteString(prefix)
	sb.WriteString(">")
	sb.WriteString(match)
	sb.WriteString("<")
	sb.WriteString(suffix)
	if to < len(text) {
		sb.WriteString("…")
	}
	return sb.String()
}

// abbreviate truncates a label to width, padding when shorter.
func abbreviate(s string, width int) string {
	if len(s) > width {
		return s[:width]
	}
	return s
}
