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
// Scope: full agent runtime (config, providers, MCP, LSP, sandbox,
// persistence, resume, agent profiles) plus the slash/overlay
// surface, all driven over the Backend seam.
//
// See ~/.claude/plans/gleaming-growing-pebble.md for the full plan.
package bubble

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"sync/atomic"

	"github.com/google/uuid"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/podman"
	"github.com/TaraTheStar/enso/internal/backend/workspace"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/permissions"
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

	// Load the user's $XDG_CONFIG_HOME/enso/theme.toml (if present) and
	// rebuild the lipgloss styles from the merged palette before any
	// rendering happens.
	loadAndApplyTheme()

	cfg, err := config.Load(cwd, opts.Config)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	providers, err := llm.BuildProviders(cfg.Providers, cfg.ResolvePools())
	if err != nil {
		return err
	}
	for _, p := range providers {
		p.IncludeProviders = cfg.Instructions.ProvidersIncluded()
	}
	defaultName, err := pickDefaultProvider(providers, cfg.DefaultProvider)
	if err != nil {
		return err
	}
	// Exactly one execution path: LocalBackend (sandbox off) or
	// PodmanBackend (sandbox on), both behind the seam. No in-process
	// branch — the structural fix the whole effort exists for.
	b, isol, bopts := host.SelectBackend(cfg)
	return runTUIViaBackend(b, isol, bopts, opts, cwd, cfg, providers, defaultName)
}

// runTUIViaBackend is the Backend-seam implementation of the
// interactive TUI for the default (sandbox-off) path. The agent core
// (model loop + tools + session message store + mcp/lsp/agents recipe)
// runs in the worker child process; the host keeps the terminal, the
// REAL providers, rendering, the permission modal, and the slash /
// overlay surface. host.Start republishes the worker's bus events onto
// busInst, so forwardBus / the conversation state machine / the
// permission modal work unchanged.
//
// Fidelity notes (the seam's honest, documented limitations — same
// philosophy as daemon-attach mode):
//   - Agent-state slash commands (/model /compact /prune /context /cost
//     /info numbers /prompt /yolo) and skills' tool-gating round-trip
//     to the worker (control RPC) or read the telemetry snapshot, so
//     they are behavior-identical.
//   - Session meta (/session /label /fork /sessions, audit, replay)
//     uses a host-opened handle to the SAME sqlite store (LocalBackend
//     shares the filesystem) — honest, not faked.
//   - Permission y/n decisions cross the wire faithfully; the modal's
//     "always"/"turn" pattern-persistence degrades to allow-once with
//     the existing notice (the enforcing checker is worker-side, like
//     attach mode). /lsp /mcp /transcript reflect worker-side managers
//     and show a host-derived view.
func runTUIViaBackend(b backend.Backend, isol backend.IsolationSpec, bopts []host.Option, opts Options, cwd string, cfg *config.Config, providers map[string]*llm.Provider, defaultName string) error {
	provider := providers[defaultName]

	// Workspace overlay: run the agent against a throwaway clone of the
	// project. The interactive commit/discard/keep prompt happens after
	// the TUI exits (terminal restored), below.
	var overlayWS *workspace.Overlay
	if pb, ok := b.(*podman.Backend); ok && cfg.Bash.Sb.Workspace == "overlay" {
		ws, err := workspace.New(context.Background(), cwd)
		if err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
		pb.MountSource = ws.Copy
		overlayWS = ws
	}

	resuming := opts.Session != ""
	sessionID := ""
	if resuming {
		sessionID = opts.Session
	} else if !opts.Ephemeral {
		sessionID = uuid.NewString()
	}

	// Non-secret provider catalog (names/models/pool only); shared
	// projection so run/TUI/daemon can't drift on what crosses the seam.
	catalog := host.ProviderCatalog(providers)

	spec := backend.TaskSpec{
		TaskID:          uuid.NewString(),
		Cwd:             cwd,
		Interactive:     true,
		Ephemeral:       opts.Ephemeral,
		MaxTurns:        opts.MaxTurns,
		Yolo:            opts.Yolo,
		AgentProfile:    opts.Agent,
		Providers:       catalog,
		DefaultProvider: defaultName,
		Isolation:       isol,
	}
	if resuming {
		spec.ResumeSessionID = opts.Session
	} else {
		spec.SessionID = sessionID
	}
	// Credential-scrub invariant: the SCRUBBED config crosses the seam.
	rc, err := json.Marshal(cfg.ScrubbedForWorker())
	if err != nil {
		return fmt.Errorf("serialize config: %w", err)
	}
	spec.ResolvedConfig = rc

	busInst := bus.New()

	// Host-side DISPLAY checker: built from the same cfg so /perms,
	// /info and the overlay reflect the policy accurately. It does NOT
	// enforce (the worker's checker does); /yolo mirrors here and
	// RPC-toggles the worker's real one.
	denies := append([]string{}, cfg.Permissions.Deny...)
	ignorePatterns, _ := permissions.LoadIgnoreFile(filepath.Join(cwd, ".ensoignore"))
	if len(ignorePatterns) > 0 {
		denies = append(denies, permissions.IgnoreToDenyPatterns(ignorePatterns)...)
	}
	dispChecker := permissions.NewChecker(cfg.Permissions.Allow, cfg.Permissions.Ask, denies, cfg.Permissions.Mode)
	if opts.Yolo {
		dispChecker.SetYolo(true)
	}

	// Host-side DISPLAY registry: the default tool set for /tools and
	// the overlay. The worker owns the real, profile-filtered registry
	// (with mcp/lsp tools); starting managers host-side would
	// double-spawn server processes, so this is intentionally the
	// unfiltered core set.
	dispRegistry := tools.BuildDefault()
	agent.RegisterSpawn(dispRegistry)
	tools.RegisterSearch(dispRegistry, cfg.Search)

	var restrictedRoots []string
	if !cfg.Permissions.DisableFileConfinement {
		restrictedRoots = append([]string{cwd}, cfg.Permissions.AdditionalDirectories...)
	}

	// Host-side handle to the SAME session store (shared fs under
	// LocalBackend) for label/fork/sessions/replay/audit. The worker
	// owns the message-append writer; this host writer only touches
	// session meta + the events (audit) table — different tables, no
	// seq contention.
	var (
		store   *session.Store
		writer  *session.Writer
		resumed *session.State
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, err := host.Start(ctx, b, spec, providers, busInst, bopts...)
	if err != nil {
		return fmt.Errorf("bubble: start worker: %w", err)
	}
	defer sess.Close()

	if !opts.Ephemeral {
		store, err = session.Open()
		if err != nil {
			return fmt.Errorf("open session store: %w", err)
		}
		defer store.Close()
		if resuming {
			resumed, err = session.Load(store, opts.Session)
			if err != nil {
				return fmt.Errorf("resume %s: %w", opts.Session, err)
			}
		} else {
			// Host owns session-row creation (mirrors the legacy
			// in-process bubble.Run). The worker attaches a
			// message-append writer to this row; the host writer below
			// is for session meta + the audit/events table.
			if _, err = session.NewSessionWithID(store, sessionID, provider.Model, defaultName, cwd); err != nil {
				return fmt.Errorf("create session: %w", err)
			}
		}
		writer, err = session.AttachWriter(store, sessionID)
		if err != nil {
			return fmt.Errorf("attach writer: %w", err)
		}
		if resuming && resumed.Interrupted {
			_ = writer.MarkInterrupted(false)
		}
	}

	printStartupBanner(provider, cfg, false, writer)
	if resumed != nil {
		replayHistory(resumed.History)
		if resumed.Interrupted {
			fmt.Println(noticeStyle.Render("(resumed: previous turn was interrupted; tool calls auto-recovered)"))
		}
	}

	inputCh := make(chan string, 64)

	// Submit pump: model writes typed/synthetic input to inputCh; we
	// relay each line to the worker as MsgInput. Mirrors the old
	// `go agt.Run(ctx, inputCh)` reader.
	submitDone := make(chan struct{})
	go func() {
		defer close(submitDone)
		for {
			select {
			case text, ok := <-inputCh:
				if !ok {
					return
				}
				if err := sess.Submit(text); err != nil {
					busInst.Publish(bus.Event{Type: bus.EventError, Payload: fmt.Errorf("submit: %w", err)})
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Audit-log subscriber (host writer → events table). Same curated
	// subset and shutdown ordering as the in-process path.
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

	// Worker lifetime: when the agent core winds down (or errors), turn
	// it into the same bus signals the conversation machine expects.
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		if werr := sess.Wait(); werr != nil && !errors.Is(werr, context.Canceled) {
			busInst.Publish(bus.Event{Type: bus.EventError, Payload: werr})
		}
	}()

	agtCtl := &sessionAgentControl{sess: sess, providers: providers}

	slashReg := slash.NewRegistry()
	slashContext := &slashCtx{
		agt:       agtCtl,
		sess:      sess,
		bus:       busInst,
		checker:   dispChecker,
		registry:  dispRegistry,
		store:     store,
		writer:    writer,
		providers: providers,
		cwd:       cwd,
		// lspMgr/mcpMgr/transcripts live worker-side; nil here → the
		// handlers' existing nil guards render a host-side view.
	}
	slashContext.submit = func(text string) {
		go func() {
			select {
			case inputCh <- text:
			case <-ctx.Done():
			}
		}()
	}
	slashContext.workflowDeps = workflow.RunDeps{
		Providers:          providers,
		DefaultProvider:    defaultName,
		Bus:                busInst,
		Registry:           dispRegistry,
		Perms:              dispChecker,
		Cwd:                cwd,
		MaxTurns:           opts.MaxTurns,
		GlobalAgents:       &atomic.Int64{}, // host-side workflow budget (agent.New resolves Max* defaults)
		Depth:              0,
		Writer:             writer,
		GitAttribution:     cfg.Git.Attribution,
		GitAttributionName: cfg.Git.AttributionName,
		WebFetchAllowHosts: cfg.WebFetch.AllowHosts,
		RestrictedRoots:    restrictedRoots,
	}
	registerBuiltins(slashReg, slashContext)

	skillSubmit := func(text string, allowedTools []string) {
		agtCtl.SetNextTurnTools(allowedTools)
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

	vim := strings.EqualFold(cfg.UI.EditorMode, "vim")
	m := &model{
		inputCh:   inputCh,
		modelName: provider.Model,
		slashReg:  slashReg,
		slashCtx:  slashContext,
		input: inputState{
			vim:       vim,
			vimNormal: vim,
		},
		overlay: &overlayData{
			agt:      agtCtl,
			cfg:      cfg,
			checker:  dispChecker,
			registry: dispRegistry,
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
	// Enforcing checker is worker-side: leaving permCheckerCwd.checker
	// nil makes the modal's "always/turn" honestly degrade to
	// allow-once (y/n still cross the wire), exactly like attach mode.
	m.permCheckerCwd.checker = nil
	m.permCheckerCwd.cwd = cwd
	slashContext.conv = &m.conv

	p := tea.NewProgram(m)
	busSub := busInst.Subscribe(8192)
	go forwardBus(p, busSub)

	_, runErr := p.Run()

	// Shutdown ordering: cancel ctx, close inputCh + tell the worker no
	// more input so it winds down, wait for the worker, then close the
	// bus (closes audit + forwardBus subscribers), drain audit, finally
	// deferred Close()s (sess teardown, store) run.
	cancel()
	close(inputCh)
	<-submitDone
	sess.CloseInput()
	<-workerDone
	busInst.Close()
	<-auditDone

	// The TUI has exited and the terminal is restored (inline mode), so
	// the overlay commit/discard/keep prompt can talk to the user
	// directly. Done before any session-switch exec so a switch can't
	// strand the diverged copy.
	if overlayWS != nil {
		_ = workspace.Resolve(context.Background(), overlayWS, true, os.Stdin, os.Stdout)
	}

	if runErr != nil {
		return fmt.Errorf("bubble: %w", runErr)
	}
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
