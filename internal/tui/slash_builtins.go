// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rivo/tview"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/agents"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/slash"
	"github.com/TaraTheStar/enso/internal/tools"
	"github.com/TaraTheStar/enso/internal/workflow"
)

// slashContext bundles the app-level objects that slash commands manipulate.
type slashContext struct {
	app         *tview.Application
	pages       *tview.Pages
	chat        *tview.TextView
	agt         *agent.Agent
	checker     *permissions.Checker
	registry    *tools.Registry
	store       *session.Store  // may be nil for ephemeral
	writer      *session.Writer // may be nil for ephemeral
	cwd         string
	activeAgent string // name of the declarative agent in use, "" = default
	runDeps     workflow.RunDeps
	stop        func()
	setMode     func(string) // status-bar mode label setter ("PROMPT" / "AUTO")

	// setSessionLabel persists a new session label and pushes it into
	// the sidebar. Callback so /rename doesn't need to know about the
	// sidebar struct directly. Returns the normalised slug actually
	// stored, or "" if input slugified to empty. Errors come from the
	// underlying writer.
	setSessionLabel func(label string) (string, error)

	// submit injects a synthetic user message into the agent's input queue,
	// applying an optional one-shot tool restriction first. This is the same
	// hook skills use; built-in commands like /init reuse it.
	submit func(text string, allowedTools []string)

	// switchSession schedules a re-exec into a different session id. Set
	// by the host (Run); used by /grep's overlay when the user picks a
	// hit. Mirrors the Ctrl-R sessions-overlay path.
	switchSession func(id string)

	// chatDisp is the live ChatDisplay for the current session. Slash
	// commands that operate on the rendered conversation (today: /find)
	// reach into it to read the block model and drive highlights.
	chatDisp *ChatDisplay
}

// registerBuiltins attaches the standard built-in commands to a slash registry.
func registerBuiltins(reg *slash.Registry, sc *slashContext) {
	reg.Register(&helpCmd{reg: reg, sc: sc})
	reg.Register(&yoloCmd{sc: sc})
	reg.Register(&toolsCmd{sc: sc})
	reg.Register(&sessionsCmd{sc: sc})
	reg.Register(&grepCmd{sc: sc})
	reg.Register(&permissionsCmd{sc: sc})
	reg.Register(&modelCmd{sc: sc})
	reg.Register(&compactCmd{sc: sc})
	reg.Register(&workflowCmd{sc: sc})
	reg.Register(&initCmd{sc: sc})
	reg.Register(&agentsCmd{sc: sc})
	reg.Register(&loopCmd{sc: sc})
	reg.Register(&renameCmd{sc: sc})
	reg.Register(&findCmd{sc: sc})
	reg.Register(&exportCmd{sc: sc})
	reg.Register(&statsCmd{sc: sc})
	reg.Register(&forkCmd{sc: sc})
	reg.Register(&quitCmd{sc: sc})
}

// pickDefaultProvider mirrors agent.pickDefaultProvider so the host can
// select an initial provider before constructing the Agent. Empty
// `requested` falls back to the alphabetically-first key — matches
// the agent's own behaviour exactly.
func pickDefaultProvider(providers map[string]*llm.Provider, requested string) (string, error) {
	if len(providers) == 0 {
		return "", fmt.Errorf("no providers configured")
	}
	if requested != "" {
		if _, ok := providers[requested]; !ok {
			return "", fmt.Errorf("default_provider %q not in [providers]", requested)
		}
		return requested, nil
	}
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names[0], nil
}

func writeChat(sc *slashContext, format string, args ...any) {
	fmt.Fprintf(sc.chat, "[gray]"+format+"[-]\n", args...)
	sc.chat.ScrollToEnd()
}

// /help

type helpCmd struct {
	reg *slash.Registry
	sc  *slashContext
}

func (c *helpCmd) Name() string        { return "help" }
func (c *helpCmd) Description() string { return "list available slash commands" }
func (c *helpCmd) Run(ctx context.Context, args string) error {
	cmds := c.reg.List()
	var b strings.Builder
	b.WriteString("Slash commands:\n")
	for _, cm := range cmds {
		fmt.Fprintf(&b, "  /%s — %s\n", cm.Name(), cm.Description())
	}
	writeChat(c.sc, "%s", b.String())
	return nil
}

// /yolo on|off

type yoloCmd struct{ sc *slashContext }

func (c *yoloCmd) Name() string        { return "yolo" }
func (c *yoloCmd) Description() string { return "toggle auto-allow mode (on|off)" }
func (c *yoloCmd) Run(ctx context.Context, args string) error {
	enable := false
	switch strings.TrimSpace(strings.ToLower(args)) {
	case "":
		// Bare /yolo flips whichever way isn't current. Previously this
		// branch always enabled, which made /yolo a one-way switch and
		// forced /yolo off to disable.
		enable = !c.sc.checker.Yolo()
	case "on", "true", "1":
		enable = true
	case "off", "false", "0":
		enable = false
	default:
		writeChat(c.sc, "yolo: usage /yolo [on|off]  (no arg toggles)")
		return nil
	}
	c.sc.checker.SetYolo(enable)
	if enable {
		c.sc.setMode("AUTO")
		writeChat(c.sc, "yolo: on (all tool calls auto-allowed)")
	} else {
		c.sc.setMode("PROMPT")
		writeChat(c.sc, "yolo: off (will prompt on unrecognised tool calls)")
	}
	return nil
}

// /tools

type toolsCmd struct{ sc *slashContext }

func (c *toolsCmd) Name() string        { return "tools" }
func (c *toolsCmd) Description() string { return "list registered tools" }
func (c *toolsCmd) Run(ctx context.Context, args string) error {
	ts := c.sc.registry.List()
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Name())
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString("Tools:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %s\n", n)
	}
	writeChat(c.sc, "%s", b.String())
	return nil
}

// /sessions

type sessionsCmd struct{ sc *slashContext }

func (c *sessionsCmd) Name() string { return "sessions" }
func (c *sessionsCmd) Description() string {
	return "list recent sessions (resume by re-running with --session <id>)"
}
func (c *sessionsCmd) Run(ctx context.Context, args string) error {
	if c.sc.store == nil {
		writeChat(c.sc, "sessions: store unavailable (running --ephemeral)")
		return nil
	}
	infos, err := session.ListRecent(c.sc.store, 20)
	if err != nil {
		return fmt.Errorf("list sessions: %w", err)
	}
	if len(infos) == 0 {
		writeChat(c.sc, "no sessions yet")
		return nil
	}
	var b strings.Builder
	b.WriteString("Recent sessions:\n")
	for _, info := range infos {
		flag := ""
		if info.Interrupted {
			flag = " [interrupted]"
		}
		fmt.Fprintf(&b, "  %s  %s  %s%s\n", info.ID, info.UpdatedAt.Format("2006-01-02 15:04"), info.Cwd, flag)
	}
	b.WriteString("\nResume: enso --session <id>\n")
	writeChat(c.sc, "%s", b.String())
	return nil
}

// /grep

type grepCmd struct{ sc *slashContext }

func (c *grepCmd) Name() string { return "grep" }
func (c *grepCmd) Description() string {
	return "search past sessions: /grep [--all] [--regex] [--text] <pattern>"
}
func (c *grepCmd) Run(ctx context.Context, args string) error {
	if c.sc.store == nil {
		writeChat(c.sc, "grep: store unavailable (running --ephemeral)")
		return nil
	}

	all, useRegex, dumpText := false, false, false
	pattern := strings.TrimSpace(args)
	for {
		switch {
		case strings.HasPrefix(pattern, "--all"):
			all = true
			pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "--all"))
		case strings.HasPrefix(pattern, "--regex"):
			useRegex = true
			pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "--regex"))
		case strings.HasPrefix(pattern, "--text"):
			dumpText = true
			pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "--text"))
		default:
			goto flagsDone
		}
	}
flagsDone:

	if !dumpText {
		// Default: open the incremental-search overlay. Pattern (if any)
		// is prepopulated; user can refine.
		ShowGrepOverlay(c.sc.app, c.sc.pages, c.sc.chat, c.sc.chat,
			c.sc.store, c.sc.cwd, pattern, useRegex, all, c.sc.switchSession)
		return nil
	}

	if pattern == "" {
		writeChat(c.sc, "usage: /grep [--all] [--regex] [--text] <pattern>")
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
			writeChat(c.sc, "grep: invalid regex: %v", reErr)
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
		writeChat(c.sc, "grep: no matches for %q %s", pattern, where)
		return nil
	}

	truncated := false
	if len(hits) > maxHits {
		hits = hits[:maxHits]
		truncated = true
	}

	var b strings.Builder
	mode := "substring"
	if useRegex {
		mode = "regex"
	}
	if all {
		fmt.Fprintf(&b, "Matches for %q [%s, all sessions]:\n", pattern, mode)
	} else {
		fmt.Fprintf(&b, "Matches for %q [%s, cwd %s]:\n", pattern, mode, c.sc.cwd)
	}
	for _, h := range hits {
		snippet := h.Snippet
		if h.Truncated {
			snippet += "  [scanned head only — message >256 KiB]"
		}
		fmt.Fprintf(&b, "  %s  %s  %s: %s\n",
			shortID(h.SessionID), relTime(h.UpdatedAt), h.Role, snippet)
	}
	if truncated {
		fmt.Fprintf(&b, "(showing first %d — narrow your query for more)\n", maxHits)
	}
	b.WriteString("\nResume: enso --session <id>\n")
	writeChat(c.sc, "%s", b.String())
	return nil
}

// /permissions

type permissionsCmd struct{ sc *slashContext }

func (c *permissionsCmd) Name() string { return "permissions" }
func (c *permissionsCmd) Description() string {
	return "list & remove project-local permission rules (config.local.toml)"
}
func (c *permissionsCmd) Run(ctx context.Context, args string) error {
	ShowPermissionsOverlay(c.sc.app, c.sc.pages, c.sc.chat, c.sc.chat, c.sc.cwd, c.sc.checker)
	return nil
}

// /model

type modelCmd struct{ sc *slashContext }

func (c *modelCmd) Name() string { return "model" }
func (c *modelCmd) Description() string {
	return "switch the active provider: /model (lists) | /model <name>"
}
func (c *modelCmd) Run(ctx context.Context, args string) error {
	args = strings.TrimSpace(args)
	cur := c.sc.agt.Provider()
	if cur == nil {
		writeChat(c.sc, "model: no active provider")
		return nil
	}
	providers := c.sc.agt.Providers
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)

	if args == "" {
		var b strings.Builder
		b.WriteString("Configured providers (current marked *):\n")
		for _, n := range names {
			marker := "  "
			if n == cur.Name {
				marker = " *"
			}
			p := providers[n]
			fmt.Fprintf(&b, "%s %-20s  %s  ctx=%s\n",
				marker, n, p.Model, formatWindow(p.ContextWindow))
		}
		b.WriteString("\nUsage: /model <name>")
		writeChat(c.sc, "%s", b.String())
		return nil
	}

	target, ok := providers[args]
	if !ok {
		writeChat(c.sc, "model: unknown provider %q (configured: %v)", args, names)
		return nil
	}
	if target.Name == cur.Name {
		writeChat(c.sc, "model: already on %q", args)
		return nil
	}
	if err := c.sc.agt.SetProvider(args); err != nil {
		writeChat(c.sc, "model: %v", err)
		return nil
	}
	writeChat(c.sc, "model: switched to %q (%s, ctx %s)", target.Name, target.Model, formatWindow(target.ContextWindow))

	// Window-asymmetry warning. Compaction trigger is at 60% of window;
	// flag if current usage is over the new threshold so the user knows
	// to /compact before the next turn.
	used := c.sc.agt.EstimateTokens()
	if target.ContextWindow > 0 && used > 0 {
		thresh := int(float64(target.ContextWindow) * 0.60)
		if used >= target.ContextWindow {
			writeChat(c.sc, "[red]warning: history (%s) exceeds %s window (%s) — next turn will fail. run /compact.[-]",
				formatWindow(used), target.Name, formatWindow(target.ContextWindow))
		} else if used > thresh {
			writeChat(c.sc, "[yellow]note: history (%s) is past %s's compaction threshold (%s of %s) — /compact recommended.[-]",
				formatWindow(used), target.Name, formatWindow(thresh), formatWindow(target.ContextWindow))
		}
	}
	return nil
}

// formatWindow renders a token count as a compact "Xk" or "X.YM"
// string for display in /model output.
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

// /compact

type compactCmd struct{ sc *slashContext }

func (c *compactCmd) Name() string { return "compact" }
func (c *compactCmd) Description() string {
	return "force a context-compaction pass on this session (with confirm-preview)"
}
func (c *compactCmd) Run(ctx context.Context, args string) error {
	preview := c.sc.agt.CompactPreview()
	if preview.NothingToDo {
		writeChat(c.sc, "compact: nothing to summarise")
		return nil
	}
	showCompactConfirm(c.sc, preview)
	return nil
}

// showCompactConfirm overlays a yes/no modal carrying the preview
// counts. On "Compact" the actual ForceCompact runs in a background
// goroutine — the LLM summary call can take seconds, and we don't
// want to block the slash dispatcher (which holds tview's event
// loop). Failure paths route results back through QueueUpdateDraw so
// chat output stays on the tview goroutine.
func showCompactConfirm(sc *slashContext, preview agent.CompactPreviewResult) {
	const pageName = "compact-confirm"
	text := fmt.Sprintf(
		"Compact this session?\n\n"+
			"  before:      ~%s tokens\n"+
			"  after (est): ~%s tokens\n"+
			"  %d message%s will be summarised\n\n"+
			"The summary call uses your current model.",
		formatThousands(preview.BeforeTokens),
		formatThousands(preview.EstAfterTokens),
		preview.MessagesToSummarise, plural(preview.MessagesToSummarise),
	)
	modal := tview.NewModal().
		SetText(text).
		AddButtons([]string{"Compact", "Cancel"})
	modal.SetDoneFunc(func(_ int, label string) {
		sc.pages.RemovePage(pageName)
		if sc.chat != nil {
			sc.app.SetFocus(sc.chat)
		}
		if label != "Compact" {
			return
		}
		go runCompactInBackground(sc)
	})
	sc.pages.AddPage(pageName, modal, true, true)
	sc.app.SetFocus(modal)
}

func runCompactInBackground(sc *slashContext) {
	did, err := sc.agt.ForceCompact(context.Background())
	sc.app.QueueUpdateDraw(func() {
		switch {
		case err != nil:
			writeChat(sc, "compact: %v", err)
		case did:
			writeChat(sc, "compaction complete")
		default:
			writeChat(sc, "compact: nothing to summarise")
		}
	})
}

// formatThousands mirrors session.formatThousands but stays local so
// slash_builtins.go doesn't need to import the format helper directly.
// 12345 → "12,345".
func formatThousands(n int) string {
	if n < 0 {
		return "-" + formatThousands(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return formatThousands(n/1000) + fmt.Sprintf(",%03d", n%1000)
}

// /workflow

type workflowCmd struct{ sc *slashContext }

func (c *workflowCmd) Name() string { return "workflow" }
func (c *workflowCmd) Description() string {
	return "run or validate a declarative workflow: /workflow <name> <args>  |  /workflow validate <name>"
}
func (c *workflowCmd) Run(ctx context.Context, args string) error {
	parts := strings.SplitN(args, " ", 2)
	if len(parts) == 0 || parts[0] == "" {
		writeChat(c.sc, "workflow: usage /workflow <name> <args>  |  /workflow validate <name>")
		return nil
	}

	// `validate` subcommand: parse the workflow without running anything
	// so authors can catch frontmatter / role / edge errors at edit
	// time rather than three minutes into a real run.
	if parts[0] == "validate" {
		rest := ""
		if len(parts) == 2 {
			rest = strings.TrimSpace(parts[1])
		}
		if rest == "" {
			writeChat(c.sc, "workflow: usage /workflow validate <name>")
			return nil
		}
		return c.validate(rest)
	}

	name := parts[0]
	rest := ""
	if len(parts) == 2 {
		rest = parts[1]
	}
	wf, err := workflow.LoadByName(c.sc.cwd, name)
	if err != nil {
		writeChat(c.sc, "workflow: %v", err)
		return nil
	}
	writeChat(c.sc, "workflow %q starting (roles: %v)", wf.Name, wf.RoleOrder)
	res, err := workflow.Run(ctx, wf, rest, c.sc.runDeps)
	if err != nil {
		writeChat(c.sc, "workflow %q: %v", wf.Name, err)
		return nil
	}
	for _, role := range wf.RoleOrder {
		out := res.Outputs[role]
		fmt.Fprintf(c.sc.chat, "[::b]%s:[::-]\n%s\n\n", role, out)
	}
	c.sc.chat.ScrollToEnd()
	return nil
}

// validate parses `name` through the same code path Run uses, but
// stops before invoking the workflow. Reports the parse error verbatim
// (the workflow package already wraps each error with role / edge /
// section context that's user-friendly enough) or a one-line summary
// on success — role list + edge count gives authors a quick visual
// sanity check that the topology matches what they expected.
func (c *workflowCmd) validate(name string) error {
	wf, err := workflow.LoadByName(c.sc.cwd, name)
	if err != nil {
		writeChat(c.sc, "workflow validate: %v", err)
		return nil
	}
	writeChat(c.sc, "workflow %q ok — %d role%s (%v), %d edge%s",
		wf.Name,
		len(wf.RoleOrder), plural(len(wf.RoleOrder)), wf.RoleOrder,
		len(wf.Edges), plural(len(wf.Edges)),
	)
	return nil
}

// /init

type initCmd struct{ sc *slashContext }

func (c *initCmd) Name() string { return "init" }
func (c *initCmd) Description() string {
	return "scan the current project and write ENSO.md with a quick orientation for future agents"
}
func (c *initCmd) Run(ctx context.Context, args string) error {
	if c.sc.submit == nil {
		writeChat(c.sc, "init: cannot submit (no input hook wired)")
		return nil
	}
	target := strings.TrimSpace(args)
	if target == "" {
		target = "ENSO.md"
	}
	prompt := initPromptTemplate(target)
	writeChat(c.sc, "init: scanning project; will propose %s", target)
	c.sc.submit(prompt, []string{"read", "grep", "glob", "write", "edit", "todo"})
	return nil
}

// initPromptTemplate is the synthetic user message /init injects. It asks
// the model to survey the project read-only first, then write a concise
// orientation doc (ENSO.md by default) at the project root.
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

// /agents

type agentsCmd struct{ sc *slashContext }

func (c *agentsCmd) Name() string { return "agents" }
func (c *agentsCmd) Description() string {
	return "list available declarative agents (built-in, ~/.enso/agents/, ./.enso/agents/)"
}
func (c *agentsCmd) Run(ctx context.Context, args string) error {
	specs, err := agents.LoadAll(c.sc.cwd)
	if err != nil {
		writeChat(c.sc, "agents: %v", err)
		return nil
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })

	active := c.sc.activeAgent
	if active == "" {
		active = "default"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Active: %s\n\nAvailable agents:\n", active)
	fmt.Fprintf(&b, "  %-12s — full default tool access\n", "default")
	for _, s := range specs {
		desc := s.Description
		if desc == "" {
			desc = "(no description)"
		}
		fmt.Fprintf(&b, "  %-12s — %s\n", s.Name, desc)
	}
	b.WriteString("\nSwitch by re-launching with --agent <name> (mid-session switch is not yet supported).\n")
	writeChat(c.sc, "%s", b.String())
	return nil
}

// /loop

// loopCmd implements `/loop <interval> <prompt>`. At most one loop is
// active per session — re-running with new args replaces the existing
// loop; running with no args stops it. The loop fires on a ticker;
// each fire injects the prompt as a synthetic user message via the
// shared submit hook (same path skills + /init use). The agent's
// per-turn busy state means the loop won't pile up if a turn takes
// longer than the interval — submit is non-blocking and the agent
// processes input in order.
type loopCmd struct {
	sc *slashContext

	mu     sync.Mutex
	cancel context.CancelFunc
}

func (c *loopCmd) Name() string { return "loop" }
func (c *loopCmd) Description() string {
	return "repeat a prompt on an interval — `/loop <interval> <prompt>` (e.g. `/loop 5m check the deploy`); `/loop` with no args stops the active loop"
}

func (c *loopCmd) Run(ctx context.Context, args string) error {
	args = strings.TrimSpace(args)
	if args == "" || strings.EqualFold(args, "off") {
		if c.stop() {
			writeChat(c.sc, "loop: stopped")
		} else {
			writeChat(c.sc, "loop: no active loop")
		}
		return nil
	}
	parts := strings.SplitN(args, " ", 2)
	if len(parts) < 2 {
		writeChat(c.sc, "loop: usage `/loop <interval> <prompt>` (e.g. `/loop 5m check the deploy`)")
		return nil
	}
	interval, err := time.ParseDuration(parts[0])
	if err != nil {
		writeChat(c.sc, "loop: invalid interval %q (try `5m`, `30s`, `1h`): %v", parts[0], err)
		return nil
	}
	if interval < 5*time.Second {
		writeChat(c.sc, "loop: interval must be at least 5s (got %s)", interval)
		return nil
	}
	prompt := strings.TrimSpace(parts[1])
	if prompt == "" {
		writeChat(c.sc, "loop: prompt is empty")
		return nil
	}
	if c.sc.submit == nil {
		writeChat(c.sc, "loop: no input hook wired")
		return nil
	}

	c.stop()
	loopCtx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancel = cancel
	c.mu.Unlock()
	go c.run(loopCtx, interval, prompt)

	writeChat(c.sc, "loop: every %s — %q (run `/loop off` to stop)", interval, truncatePrompt(prompt, 60))
	return nil
}

func (c *loopCmd) run(ctx context.Context, interval time.Duration, prompt string) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sc.submit(prompt, nil)
		}
	}
}

// stop cancels the active loop if any. Returns true when a loop was
// actually stopped.
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

func truncatePrompt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// /rename

type renameCmd struct{ sc *slashContext }

func (c *renameCmd) Name() string { return "rename" }
func (c *renameCmd) Description() string {
	return "show or override the session's display label: /rename (shows current) | /rename <label>"
}
func (c *renameCmd) Run(ctx context.Context, args string) error {
	args = strings.TrimSpace(args)
	if args == "" {
		// No-arg: report current. Read from the writer when available
		// so the answer reflects the persisted truth even if a recent
		// auto-label hasn't yet propagated to the sidebar.
		current := ""
		if c.sc.writer != nil {
			if got, err := c.sc.writer.Label(); err == nil {
				current = got
			}
		}
		if current == "" {
			writeChat(c.sc, "rename: no label set yet — usage /rename <label>")
		} else {
			writeChat(c.sc, "rename: current label %q (override with /rename <label>)", current)
		}
		return nil
	}
	if c.sc.setSessionLabel == nil {
		writeChat(c.sc, "rename: unavailable in this session")
		return nil
	}
	slug, err := c.sc.setSessionLabel(args)
	if err != nil {
		writeChat(c.sc, "rename: %v", err)
		return nil
	}
	if slug == "" {
		writeChat(c.sc, "rename: %q has no usable characters (alphanumerics required)", args)
		return nil
	}
	writeChat(c.sc, "rename: label set to %q", slug)
	return nil
}

// /find

type findCmd struct{ sc *slashContext }

func (c *findCmd) Name() string { return "find" }
func (c *findCmd) Description() string {
	return "search the current chat: /find [-e] <pattern> (Ctrl-F also opens this overlay)"
}
func (c *findCmd) Run(ctx context.Context, args string) error {
	if c.sc.chatDisp == nil {
		writeChat(c.sc, "find: chat display unavailable")
		return nil
	}
	useRegex := false
	pattern := strings.TrimSpace(args)
	// Single-flag parser: `-e` switches to regex. /grep accepts longer
	// flags (--regex, --all, --text); /find has just the one toggle so
	// the short form keeps the common case fast to type.
	if strings.HasPrefix(pattern, "-e ") || pattern == "-e" {
		useRegex = true
		pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "-e"))
	}
	ShowFindOverlay(c.sc.app, c.sc.pages, c.sc.chat, c.sc.chatDisp, pattern, useRegex)
	return nil
}

// /export

type exportCmd struct{ sc *slashContext }

func (c *exportCmd) Name() string { return "export" }
func (c *exportCmd) Description() string {
	return "save the current session as markdown: /export [path] (default: <cwd>/.enso/exports/)"
}
func (c *exportCmd) Run(ctx context.Context, args string) error {
	if c.sc.store == nil {
		writeChat(c.sc, "export: store unavailable (running --ephemeral)")
		return nil
	}
	if c.sc.agt == nil || c.sc.agt.AgentCtx == nil {
		writeChat(c.sc, "export: agent unavailable")
		return nil
	}
	sessionID := c.sc.agt.AgentCtx.SessionID
	if sessionID == "" {
		writeChat(c.sc, "export: no session id")
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
			writeChat(c.sc, "export: mkdir: %v", err)
			return nil
		}
		// Best-effort: ensure the exports/ subtree is gitignored so a
		// project doesn't accidentally check in transcripts. Failure
		// is a soft warning — the export still proceeds.
		if err := ensureEnsoGitignore(c.sc.cwd); err != nil {
			writeChat(c.sc, "export: could not write .enso/.gitignore: %v", err)
		}
		path = filepath.Join(dir, short+".md")
	}
	f, err := os.Create(path)
	if err != nil {
		writeChat(c.sc, "export: create %s: %v", path, err)
		return nil
	}
	defer f.Close()
	if err := session.WriteMarkdownByID(f, c.sc.store, sessionID); err != nil {
		writeChat(c.sc, "export: %v", err)
		return nil
	}
	writeChat(c.sc, "export: wrote %s", path)
	return nil
}

// ensureEnsoGitignore guarantees that <cwd>/.enso/.gitignore exists
// and lists `exports/`. Idempotent — re-running does nothing if the
// rule is already present. Uses a nested .gitignore so we don't have
// to modify whatever rules the user keeps at the repo root.
func ensureEnsoGitignore(cwd string) error {
	dir := filepath.Join(cwd, ".enso")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, ".gitignore")
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "exports/" {
			return nil
		}
	}
	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(prefix + "exports/\n")
	return err
}

// /stats

type statsCmd struct{ sc *slashContext }

func (c *statsCmd) Name() string { return "stats" }
func (c *statsCmd) Description() string {
	return "session activity summary: /stats [--all]"
}
func (c *statsCmd) Run(ctx context.Context, args string) error {
	args = strings.TrimSpace(args)
	if args == "--all" {
		if c.sc.store == nil {
			writeChat(c.sc, "stats: store unavailable (running --ephemeral)")
			return nil
		}
		st, err := session.ComputeStats(c.sc.store, time.Time{})
		if err != nil {
			writeChat(c.sc, "stats: %v", err)
			return nil
		}
		var buf strings.Builder
		if err := session.WriteStatsText(&buf, st, time.Time{}); err != nil {
			writeChat(c.sc, "stats: render: %v", err)
			return nil
		}
		// Stats text already carries newlines and labels — bypass the
		// gray-prefix wrapper so multi-line output isn't all rendered
		// on one wrapped line.
		fmt.Fprintf(c.sc.chat, "[gray]%s[-]\n", buf.String())
		c.sc.chat.ScrollToEnd()
		return nil
	}
	if args != "" {
		writeChat(c.sc, "stats: usage /stats [--all]")
		return nil
	}
	c.writeCurrentSessionStats()
	return nil
}

// writeCurrentSessionStats reports the live agent's history without
// hitting the DB — message-by-role counts and the same 4-char token
// estimate the sidebar uses, so the numbers agree across surfaces.
func (c *statsCmd) writeCurrentSessionStats() {
	if c.sc.agt == nil {
		writeChat(c.sc, "stats: agent unavailable")
		return
	}
	hist := c.sc.agt.History
	byRole := map[string]int{}
	toolCalls := 0
	for _, m := range hist {
		byRole[m.Role]++
		toolCalls += len(m.ToolCalls)
	}
	tokens := llm.Estimate(hist)
	window := c.sc.agt.ContextWindow()

	var b strings.Builder
	fmt.Fprintf(&b, "Current session:\n")
	fmt.Fprintf(&b, "  Messages: %d  (user %d, assistant %d, tool %d, system %d)\n",
		len(hist), byRole["user"], byRole["assistant"], byRole["tool"], byRole["system"])
	fmt.Fprintf(&b, "  Tool calls: %d\n", toolCalls)
	if window > 0 {
		pct := float64(tokens) / float64(window) * 100
		fmt.Fprintf(&b, "  Tokens (~est): %d / %d  (%.0f%% of context window)\n", tokens, window, pct)
	} else {
		fmt.Fprintf(&b, "  Tokens (~est): %d\n", tokens)
	}
	fmt.Fprintf(&b, "  (use /stats --all for cross-session totals)")
	fmt.Fprintf(c.sc.chat, "[gray]%s[-]\n", b.String())
	c.sc.chat.ScrollToEnd()
}

// /fork

type forkCmd struct{ sc *slashContext }

func (c *forkCmd) Name() string { return "fork" }
func (c *forkCmd) Description() string {
	return "branch a session: /fork [id]  (defaults to current session)"
}
func (c *forkCmd) Run(ctx context.Context, args string) error {
	if c.sc.store == nil {
		writeChat(c.sc, "fork: store unavailable (running --ephemeral)")
		return nil
	}
	src := strings.TrimSpace(args)
	if src == "" {
		if c.sc.agt == nil || c.sc.agt.AgentCtx == nil {
			writeChat(c.sc, "fork: no session id")
			return nil
		}
		src = c.sc.agt.AgentCtx.SessionID
	}
	if src == "" {
		writeChat(c.sc, "fork: no session id")
		return nil
	}
	newID, err := session.Fork(c.sc.store, src)
	if err != nil {
		writeChat(c.sc, "fork: %v", err)
		return nil
	}
	writeChat(c.sc, "fork: created session %s", newID)
	writeChat(c.sc, "       resume with:  enso --session %s", newID)
	return nil
}

// /quit

type quitCmd struct{ sc *slashContext }

func (c *quitCmd) Name() string        { return "quit" }
func (c *quitCmd) Description() string { return "exit enso" }
func (c *quitCmd) Run(ctx context.Context, args string) error {
	c.sc.stop()
	return nil
}
