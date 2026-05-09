// SPDX-License-Identifier: AGPL-3.0-or-later

// Package bubble is the scrollback-native Bubble Tea backend for the
// enso UI. It runs in inline mode (no alt-screen): completed assistant
// messages are flushed to the terminal's real scrollback via tea.Println,
// and a small live region at the bottom holds the currently-streaming
// message, a single-line status, and the input prompt.
//
// Once a message finishes streaming it graduates to scrollback, where
// terminal-native selection works like in any tmux pane.
//
// Phase 1+ scope: full agent runtime (config, providers, MCP, LSP,
// sandbox, persistence, resume, agent profiles). Yolo-only — the
// permission flow lands in phase 5. UI features (sidebar, vim, slash
// commands, overlays) land in phases 3-5.
//
// See ~/.claude/plans/gleaming-growing-pebble.md for the full plan.
package bubble

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/agents"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/hooks"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/lsp"
	"github.com/TaraTheStar/enso/internal/mcp"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/sandbox"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/slash"
	"github.com/TaraTheStar/enso/internal/tools"
	"github.com/TaraTheStar/enso/internal/ui/blocks"
	"github.com/TaraTheStar/enso/internal/workflow"
)

// Options mirrors internal/ui.Options.
type Options struct {
	Yolo      bool
	Session   string
	Ephemeral bool
	MaxTurns  int
	Config    string
	Agent     string
}

// Run is the entry point for an interactive session. It builds the
// runtime layer (providers, MCP, LSP, sandbox, sessions, agent) and
// hands streaming events to the Bubble Tea program for rendering.
func Run(opts Options) error {
	// Yolo skips the permission prompt entirely; non-yolo runs through
	// the inline y/n/a/t flow in model.handleBusEvent.

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}

	// Load the user's ~/.enso/theme.toml (if present) and rebuild the
	// lipgloss styles from the merged palette before any rendering
	// happens.
	loadAndApplyTheme()

	cfg, err := config.Load(cwd, opts.Config)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	providers, err := llm.BuildProviders(cfg.Providers)
	if err != nil {
		return err
	}
	defaultName, err := pickDefaultProvider(providers, cfg.DefaultProvider)
	if err != nil {
		return err
	}
	provider := providers[defaultName]

	busInst := bus.New()

	denies := append([]string{}, cfg.Permissions.Deny...)
	ignorePatterns, _ := permissions.LoadIgnoreFile(filepath.Join(cwd, ".ensoignore"))
	if len(ignorePatterns) > 0 {
		denies = append(denies, permissions.IgnoreToDenyPatterns(ignorePatterns)...)
	}
	checker := permissions.NewChecker(cfg.Permissions.Allow, cfg.Permissions.Ask, denies, cfg.Permissions.Mode)
	if opts.Yolo {
		checker.SetYolo(true)
	}

	registry := tools.BuildDefault()
	agent.RegisterSpawn(registry)
	tools.RegisterSearch(registry, cfg.Search)

	transcripts := tools.NewTranscripts()

	mcpMgr := mcp.NewManager()
	if len(cfg.MCP) > 0 {
		mcpMgr.Start(context.Background(), cfg.MCP)
		mcpMgr.RegisterAll(registry)
	}
	defer mcpMgr.Close()

	lspMgr := lsp.NewManager(cwd, cfg.LSP)
	tools.RegisterLSP(registry, lspMgr)
	defer lspMgr.Close()

	var sandboxMgr *sandbox.Manager
	if sbCfg, on := sandbox.FromConfig(cfg); on {
		sandboxMgr, err = sandbox.NewManager(cwd, sbCfg)
		if err != nil {
			return fmt.Errorf("sandbox: %w", err)
		}
		ensureCtx, ensureCancel := context.WithTimeout(context.Background(), 60*time.Second)
		if err := sandboxMgr.Ensure(ensureCtx, os.Stderr); err != nil {
			ensureCancel()
			return fmt.Errorf("sandbox: ensure %s: %w", sandboxMgr.ContainerName(), err)
		}
		ensureCancel()
	}
	var restrictedRoots []string
	if !cfg.Permissions.DisableFileConfinement {
		restrictedRoots = append([]string{cwd}, cfg.Permissions.AdditionalDirectories...)
	}

	spec, err := agents.Find(cwd, opts.Agent)
	if err != nil {
		return err
	}
	applied := agents.Apply(spec, provider, registry)
	provider = applied.Provider
	registry = applied.Registry

	var (
		store   *session.Store
		writer  *session.Writer
		resumed *session.State
	)
	if !opts.Ephemeral {
		store, err = session.Open()
		if err != nil {
			return fmt.Errorf("open session store: %w", err)
		}
		defer store.Close()

		if opts.Session != "" {
			resumed, err = session.Load(store, opts.Session)
			if err != nil {
				return fmt.Errorf("resume %s: %w", opts.Session, err)
			}
			writer, err = session.AttachWriter(store, opts.Session)
			if err != nil {
				return fmt.Errorf("attach writer: %w", err)
			}
			if resumed.Interrupted {
				_ = writer.MarkInterrupted(false)
			}
		} else {
			writer, err = session.NewSession(store, provider.Model, provider.Name, cwd)
			if err != nil {
				return fmt.Errorf("create session: %w", err)
			}
		}
	}

	maxTurns := opts.MaxTurns
	if applied.MaxTurns > 0 {
		maxTurns = applied.MaxTurns
	}

	hooksInst := hooks.New(cfg.Hooks.OnFileEdit, cfg.Hooks.OnSessionEnd)

	agentCfg := agent.Config{
		Providers:             providers,
		DefaultProvider:       defaultName,
		Bus:                   busInst,
		Registry:              registry,
		Perms:                 checker,
		Writer:                writer,
		Cwd:                   cwd,
		MaxTurns:              maxTurns,
		Transcripts:           transcripts,
		GitAttribution:        cfg.Git.Attribution,
		GitAttributionName:    cfg.Git.AttributionName,
		ExtraSystemPrompt:     applied.PromptAppend,
		AdditionalDirectories: cfg.Permissions.AdditionalDirectories,
		RestrictedRoots:       restrictedRoots,
		Hooks:                 hooksInst,
		WebFetchAllowHosts:    cfg.WebFetch.AllowHosts,
	}
	if sandboxMgr != nil {
		agentCfg.Sandbox = sandboxMgr
	}
	if writer != nil {
		agentCfg.SessionID = writer.SessionID()
	}
	if resumed != nil {
		agentCfg.History = resumed.History
	}

	agt, err := agent.New(agentCfg)
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	// Anything printed via fmt.Println BEFORE tea.NewProgram.Run() lands
	// in the user's real terminal scrollback (we're inline, not
	// alt-screen). Use that to surface the startup state — model,
	// MCP/LSP/sandbox configuration — so the user can see what runtime
	// services are loading without summoning the inspector overlay.
	printStartupBanner(provider, cfg, sandboxMgr != nil, writer)

	// Replay the resumed transcript into terminal scrollback before the
	// program takes over. The phase-1 renderer is intentionally terse;
	// phase 3 reuses the shared block-model formatter for styled,
	// consistent rendering between live and replayed messages.
	if resumed != nil {
		replayHistory(resumed.History)
		if resumed.Interrupted {
			fmt.Println(noticeStyle.Render("(resumed: previous turn was interrupted; tool calls auto-recovered)"))
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inputCh := make(chan string, 64)

	// Audit-log subscriber: persist a curated subset of events into the
	// session's events table so diagnostic replay is possible. Subscribes
	// before the agent goroutine starts so we don't miss the first
	// EventUserMessage. auditDone guards the shutdown ordering — we
	// drain audit before closing the store.
	auditDone := make(chan struct{})
	if writer != nil {
		auditCh := busInst.Subscribe(64)
		go func() {
			defer close(auditDone)
			for evt := range auditCh {
				typ := auditTypeName(evt.Type)
				if typ == "" {
					continue
				}
				if err := writer.AppendEvent(typ, sanitizePayload(evt.Payload)); err != nil {
					slog.Warn("session: append event", "type", typ, "err", err)
				}
			}
		}()
	} else {
		close(auditDone)
	}

	agentDone := make(chan struct{})
	go func() {
		defer close(agentDone)
		if err := agt.Run(ctx, inputCh); err != nil && err != context.Canceled {
			busInst.Publish(bus.Event{Type: bus.EventError, Payload: err})
		}
	}()

	// Slash command registry + handler context. The handlers are wired
	// with concrete refs to the agent / checker / store / providers so
	// /yolo, /model, /info, etc. has everything it needs to run inline.
	slashReg := slash.NewRegistry()
	slashContext := &slashCtx{
		agt:         agt,
		checker:     checker,
		registry:    registry,
		store:       store,
		writer:      writer,
		providers:   providers,
		cwd:         cwd,
		lspMgr:      lspMgr,
		mcpMgr:      mcpMgr,
		transcripts: transcripts,
	}
	// submit is the hook /init and /loop use to push a synthetic user
	// message into the agent's input channel.
	slashContext.submit = func(text string) {
		go func() {
			select {
			case inputCh <- text:
			case <-ctx.Done():
			}
		}()
	}

	// workflowDeps wires every cross-cutting runtime reference /workflow
	// needs into one bag. Pre-built so individual handler invocations
	// don't re-derive them.
	slashContext.workflowDeps = workflow.RunDeps{
		Providers:          providers,
		DefaultProvider:    defaultName,
		Bus:                busInst,
		Registry:           registry,
		Perms:              checker,
		Cwd:                cwd,
		MaxTurns:           maxTurns,
		GlobalAgents:       agt.AgentCtx.GlobalAgents,
		MaxAgents:          agt.AgentCtx.MaxAgents,
		MaxDepth:           agt.AgentCtx.MaxDepth,
		Depth:              0,
		Transcripts:        transcripts,
		Writer:             writer,
		GitAttribution:     cfg.Git.Attribution,
		GitAttributionName: cfg.Git.AttributionName,
		WebFetchAllowHosts: cfg.WebFetch.AllowHosts,
		RestrictedRoots:    restrictedRoots,
	}

	registerBuiltins(slashReg, slashContext)

	// Custom skills from ~/.enso/skills and ./.enso/skills. Each skill
	// renders a templated prompt and (optionally) restricts the next
	// turn to a subset of tools. The submitter applies the restriction
	// before pushing the rendered text into inputCh.
	skillSubmit := func(text string, allowedTools []string) {
		agt.SetNextTurnTools(allowedTools)
		go func() {
			select {
			case inputCh <- text:
			case <-ctx.Done():
			}
		}()
	}
	if skills, err := slash.LoadSkills(cwd); err != nil {
		fmt.Println(noticeStyle.Render(fmt.Sprintf("skills load: %v", err)))
	} else {
		for _, sk := range skills {
			sk.SetSubmitter(skillSubmit)
			slashReg.Register(sk)
		}
	}

	// conv is held by the model; the slashCtx grabs the same pointer
	// after construction so /find can see history.

	// Vim mode is enabled when [ui] editor_mode = "vim" in config.
	// The single-line subset is h/l/0/$/w/b/x/i/a/A; j/k/G/o/O don't
	// apply since the input is one line.
	vim := strings.EqualFold(cfg.UI.EditorMode, "vim")

	m := &model{
		inputCh:   inputCh,
		modelName: agt.Provider().Model,
		slashReg:  slashReg,
		slashCtx:  slashContext,
		input: inputState{
			vim:       vim,
			vimNormal: vim, // start in normal mode when vim is on
		},
		overlay: &overlayData{
			agt:      agt,
			cfg:      cfg,
			checker:  checker,
			registry: registry,
			writer:   writer,
			cwd:      cwd,
		},
		picker: &pickerData{
			cwd:            cwd,
			extraDirs:      cfg.Permissions.AdditionalDirectories,
			ignorePatterns: ignorePatterns,
		},
		sessions: &sessionsOverlayData{store: store},
	}
	m.permCheckerCwd.checker = checker
	m.permCheckerCwd.cwd = cwd
	slashContext.conv = &m.conv
	p := tea.NewProgram(m)

	busSub := busInst.Subscribe(8192)
	go forwardBus(p, busSub)

	_, runErr := p.Run()

	// Shutdown ordering: cancel ctx so the agent loop returns, close
	// inputCh so any pending submit unwinds, wait for the agent
	// goroutine, then close the bus (which closes every subscriber
	// channel including audit), wait for audit drain, finally let
	// deferred Close() calls (mcpMgr, lspMgr, store) run.
	cancel()
	close(inputCh)
	<-agentDone
	busInst.Close()
	<-auditDone

	if runErr != nil {
		return fmt.Errorf("bubble: %w", runErr)
	}

	// If the user picked a session in the Ctrl-R overlay, syscall.Exec
	// into the same binary with --session <id>. Never returns on
	// success — the new process replaces ours.
	if m.pendingSwitch != "" {
		return execIntoSession(m.pendingSwitch)
	}
	return nil
}

// forwardBus reads bus events and sends them into the program as
// busEventMsg. Coalesces at 16ms (~60fps) — without it, fast streams
// (>100 t/s) would flood program.Send and bog the renderer.
func forwardBus(p *tea.Program, sub <-chan bus.Event) {
	const flushInterval = 16 * time.Millisecond
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	var pending []bus.Event
	flush := func() {
		if len(pending) == 0 {
			return
		}
		for _, ev := range pending {
			p.Send(busEventMsg{ev: ev})
		}
		pending = pending[:0]
	}
	for {
		select {
		case ev, ok := <-sub:
			if !ok {
				flush()
				return
			}
			pending = append(pending, ev)
		case <-ticker.C:
			flush()
		}
	}
}

// pickDefaultProvider mirrors internal/agent.pickDefaultProvider; we
// replicate it here because the agent's copy is unexported.
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
	var first string
	for name := range providers {
		if first == "" || strings.Compare(name, first) < 0 {
			first = name
		}
	}
	return first, nil
}

// printStartupBanner prints a one-or-two-line banner describing the
// session that's about to start. The banner is the user's first
// confirmation that MCP/LSP/sandbox are spinning up; the same data
// is also reachable on demand via /info or the Ctrl-Space overlay.
func printStartupBanner(provider *llm.Provider, cfg *config.Config, sandboxOn bool, writer *session.Writer) {
	header := asstStyle.Render("enso") + "  " + provider.Model
	if writer != nil {
		header += statusStyle.Render("  · " + shortID(writer.SessionID()))
	} else {
		header += statusStyle.Render("  · ephemeral")
	}
	fmt.Println(header)

	var bits []string
	if n := len(cfg.MCP); n > 0 {
		names := make([]string, 0, n)
		for name := range cfg.MCP {
			names = append(names, name)
		}
		bits = append(bits, fmt.Sprintf("mcp: %s", strings.Join(names, ", ")))
	}
	if n := len(cfg.LSP); n > 0 {
		names := make([]string, 0, n)
		for name := range cfg.LSP {
			names = append(names, name)
		}
		bits = append(bits, fmt.Sprintf("lsp: %s", strings.Join(names, ", ")))
	}
	if sandboxOn {
		bits = append(bits, "sandbox: on")
	}
	if len(bits) > 0 {
		fmt.Println(statusStyle.Render("→ " + strings.Join(bits, " · ")))
	}
}

func shortID(id string) string {
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

// replayHistory prints a resumed session's history into terminal
// scrollback using the same block renderer that drives the live region,
// so replayed and live messages look identical. Tool-call and
// reasoning messages from history are skipped for now — re-rendering
// a tool result without its progress sequence loses information, and
// the existing message log doesn't carry tool block boundaries.
func replayHistory(history []llm.Message) {
	for _, msg := range history {
		var b blocks.Block
		switch msg.Role {
		case "user":
			b = &blocks.User{Text: msg.Content}
		case "assistant":
			b = &blocks.Assistant{Text: msg.Content}
		}
		if b == nil {
			continue
		}
		if s := renderBlock(b, 0, true); s != "" {
			fmt.Println(s)
		}
	}
}

// auditTypeName / sanitizePayload classify and coerce bus event
// payloads for persistence in the session events table. typ=="" for
// events we deliberately don't audit (per-token deltas, permission
// channels, etc.).
func auditTypeName(t bus.EventType) string {
	switch t {
	case bus.EventUserMessage:
		return "UserMessage"
	case bus.EventAssistantDone:
		return "AssistantDone"
	case bus.EventToolCallStart:
		return "ToolCallStart"
	case bus.EventToolCallEnd:
		return "ToolCallEnd"
	case bus.EventCancelled:
		return "Cancelled"
	case bus.EventError:
		return "Error"
	case bus.EventCompacted:
		return "Compacted"
	case bus.EventAgentStart:
		return "AgentStart"
	case bus.EventAgentEnd:
		return "AgentEnd"
	case bus.EventPermissionAuto:
		return "PermissionAuto"
	}
	return ""
}

func sanitizePayload(p any) any {
	switch v := p.(type) {
	case nil:
		return nil
	case error:
		return v.Error()
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, val := range v {
			out[k] = sanitizePayload(val)
		}
		return out
	default:
		return v
	}
}
