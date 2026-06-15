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
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/google/uuid"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/backend/workspace"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/slash"
	"github.com/TaraTheStar/enso/internal/tools"
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
	b, isol, bopts := host.SelectBackend(cfg, opts.Yolo, true /* attended TUI: a denied egress can prompt the operator */)
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
//     attach mode). /lsp /mcp reflect worker-side managers
//     and show a host-derived view.
func runTUIViaBackend(b backend.Backend, isol backend.IsolationSpec, bopts []host.Option, opts Options, cwd string, cfg *config.Config, providers map[string]*llm.Provider, defaultName string) error {
	provider := providers[defaultName]

	// Workspace overlay: run the agent against a throwaway clone of the
	// project. The interactive commit/discard/keep prompt happens after
	// the TUI exits (terminal restored), below.
	overlayWS, werr := host.SetupWorkspaceOverlay(context.Background(), b, cfg, cwd, os.Stderr)
	if werr != nil {
		return fmt.Errorf("workspace: %w", werr)
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
	sc := cfg.ScrubbedForWorker()
	// Resolve searxng ca_cert into bytes that survive the seam (a sealed
	// worker has no host config dir mounted); a read failure warns on
	// HOST stderr, uniformly visible for every backend.
	sc.ResolveSearchSecrets(os.Stderr)
	rc, err := json.Marshal(sc)
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
	ignorePath := filepath.Join(cwd, ".ensoignore")
	ignorePatterns, ierr := permissions.LoadIgnoreFile(ignorePath)
	if ierr != nil {
		// Fail loud, not open: LoadIgnoreFile returns (nil, nil) for a
		// missing file, so any error here means the file exists but
		// couldn't be read or scanned — its deny rules would otherwise
		// be silently dropped. Keep whatever partial patterns we got.
		slog.Warn(".ensoignore unreadable — ignore-derived deny rules may be missing",
			"path", ignorePath, "err", ierr)
	}
	if len(ignorePatterns) > 0 {
		denies = append(denies, permissions.IgnoreToDenyPatterns(ignorePatterns, cwd)...)
	}
	dispChecker := permissions.NewChecker(cfg.Permissions.Allow, cfg.Permissions.Ask, denies, cfg.Permissions.Mode)
	// Canonicalize path args against the session cwd, mirroring the
	// worker's enforcing checker.
	dispChecker.SetCwd(cwd)
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

	// Session store/row/writer are set up BEFORE host.Start so the
	// host-side writer can be threaded into the Session via
	// host.WithWriter: an ISOLATED worker (podman/lima) cannot write
	// the host DB itself, so it ships each append over the seam and the
	// host applies it through this writer. The LOCAL worker still
	// writes the shared DB directly (WithWriter is then inert — no
	// persist envelopes arrive). resumed history is shipped to the
	// worker via spec.ResumeHistory (an isolated worker's own guest DB
	// is empty so it can't session.Load; the local worker ignores it).
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
			// in-process bubble.Run). This host writer also serves
			// session meta + the audit/events table.
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
		if resumed != nil {
			if rh, mErr := json.Marshal(resumed.History); mErr == nil {
				spec.ResumeHistory = rh
			}
			if len(resumed.MessageUsage) > 0 {
				if rmu, mErr := json.Marshal(resumed.MessageUsage); mErr == nil {
					spec.ResumeMessageUsage = rmu
				}
			}
			if resumed.LastUsage != nil {
				if rlu, mErr := json.Marshal(resumed.LastUsage); mErr == nil {
					spec.ResumeLastUsage = rlu
				}
			}
		}
		bopts = append(bopts, host.WithWriter(writer))
		// Isolated-backend /rewind checkpointing: when the agent works in
		// an overlay (podman/lima), the host snapshots its `merged` dir per
		// user turn (the guest can't reach it). Local has no overlay here
		// (overlayWS == nil) — the worker snapshots the real tree itself.
		if overlayWS != nil && !cfg.Checkpoints.Disabled {
			bopts = append(bopts, host.WithCheckpointer(store, overlayWS.Copy, cfg.Checkpoints.RetainOrDefault()))
		}
	}

	sess, err := host.Start(ctx, b, spec, providers, busInst, bopts...)
	if err != nil {
		return fmt.Errorf("bubble: start worker: %w", err)
	}
	defer sess.Close()
	host.RecordWorkerAttach(writer, b, isol, spec.TaskID)

	// SIGHUP (terminal/tab closed, parent shell exited) is NOT handled
	// by bubbletea and its default action kills enso instantly, so the
	// `defer sess.Close()` above never runs — and the backend worker
	// process group was deliberately Setpgid'd off our terminal so a
	// terminal SIGHUP wouldn't reach it. Net: nothing reaps it and the
	// lima `limactl`/ssh session into the VM stays open. So on SIGHUP we
	// run Teardown explicitly, then exit. (SIGINT/SIGTERM are left to
	// bubbletea, which returns from p.Run() and restores the terminal,
	// after which the deferred sess.Close() tears down — handling them
	// here too would skip that terminal restore.)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	go func() {
		if _, ok := <-hup; !ok {
			return
		}
		_ = sess.Close() // idempotent Teardown: reaps limactl + ssh
		os.Exit(129)     // 128 + SIGHUP
	}()

	printStartupBanner(provider, cfg, writer)
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
				// Resolve `@path` image mentions to bytes HOST-side: the
				// worker (especially isolated) can't read the host FS where
				// the file lives, so the image must cross the seam as bytes.
				// The `@path` text stays in the message as a reference.
				images, attached, problems := resolveImageMentions(text, cwd)
				for _, p := range problems {
					busInst.Publish(bus.Event{Type: bus.EventError, Payload: fmt.Errorf("%s", p)})
				}
				if notice := imageAttachNotice(attached); notice != "" {
					busInst.Publish(bus.Event{Type: bus.EventNotice, Payload: notice})
				}
				if err := sess.Submit(text, images); err != nil {
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
				appendAuditEvent(writer, evt)
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
		// lspMgr/mcpMgr live worker-side; nil here → the
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
			// Skills must not shadow built-ins: a cloned repo's
			// ./.enso/skills/<name>.md could otherwise hijack a command
			// like /quit. Built-ins are registered above and win.
			if !slashReg.RegisterIfAbsent(sk) {
				fmt.Println(noticeStyle.Render(fmt.Sprintf(
					"skill /%s shadows a built-in command — skipped", sk.Name())))
			}
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
		sessions:   &sessionsOverlayData{store: store, cwd: cwd},
		palette:    &slashPaletteData{reg: slashReg},
		rewind:     &rewindOverlayData{store: store, sessionID: sessionID},
		cancelTurn: sess.Cancel,
	}
	// Re-drop a rewound turn's prompt: a /rewind of the conversation
	// re-execs into this session with the rewound-away text staged in an
	// env var, so the resumed input box pre-fills it (the user re-sends or
	// edits). Consume it once so it doesn't leak into a later re-exec.
	if v := os.Getenv(rewindPromptEnv); v != "" {
		m.input.insertString(v)
		_ = os.Unsetenv(rewindPromptEnv)
	}
	// The enforcing checker is worker-side; dispChecker is its host
	// display mirror. "always"/"turn" grants from the modal apply to
	// the worker's real checker over the seam (m.permCheckerCwd.sess,
	// mirroring how /yolo RPCs the enforcing checker) and mirror onto
	// dispChecker so /perms + /info stay in sync — so they actually
	// gate future calls instead of degrading to allow-once.
	m.permCheckerCwd.checker = dispChecker
	m.permCheckerCwd.cwd = cwd
	m.permCheckerCwd.sess = sess
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
	// CloseInput only SENDS MsgShutdown; it does not close the Channel.
	// workerDone closes only when sess.Wait() returns, i.e. when the
	// in-guest worker EOFs the Channel. With an isolated backend that
	// transport is an SSH session multiplexed over Lima's persistent
	// ControlMaster mux: if the worker doesn't wind down promptly the
	// limactl-shell stdout pipe never EOFs and this blocks FOREVER —
	// enso never returns from main and the launching shell can't get
	// its prompt back ("exit hangs"). The forced Channel close + pgroup
	// kill that breaks that deadlock lives in Teardown (deferred
	// sess.Close()), which is scheduled AFTER this wait — so bound the
	// graceful wind-down and, on timeout, run the idempotent Teardown
	// here to force the seam loop's Channel read to return.
	select {
	case <-workerDone:
	case <-time.After(3 * time.Second):
		_ = sess.Close() // idempotent; closes Channel + reaps limactl+ssh
		<-workerDone
	}
	busInst.Close()
	<-auditDone

	// The TUI has exited and the terminal is restored (inline mode), so
	// the overlay commit/discard/keep prompt can talk to the user
	// directly. Done before any session-switch exec so a switch can't
	// strand the diverged copy.
	if overlayWS != nil {
		_ = workspace.Resolve(context.Background(), overlayWS, true, os.Stdin, os.Stdout,
			workspace.WithDiffStyler(StyleDiffBlob))
	}

	if runErr != nil {
		return fmt.Errorf("bubble: %w", runErr)
	}
	if m.pendingRewind != nil {
		// Restore files and/or conversation, then re-exec into this
		// (possibly truncated) session. Done after teardown so no live
		// worker races the FS restore and the reloaded worker sees the
		// truncated history. Never returns on success (syscall.Exec).
		return performRewind(*m.pendingRewind, store, sessionID, cwd)
	}
	if m.pendingNew {
		// /new: re-exec with session-selecting flags stripped so the new
		// process mints a fresh session. Done after teardown so the old
		// worker is fully wound down first.
		return execIntoNewSession()
	}
	if m.pendingSwitch != "" {
		return execIntoSession(m.pendingSwitch)
	}
	return nil
}

// forwardBus reads bus events and sends them into the program as
// busEventMsg. Coalesces at 16ms (~60fps): each flush merges runs of
// consecutive same-type streaming deltas into one event (see
// coalesceDeltas), so a fast stream (>100 t/s) costs one program
// message — one Update()/View() — per frame per stream type instead of
// one per token. The flush timer is armed only while events are
// pending, so an idle session takes no periodic wakeups.
func forwardBus(p *tea.Program, sub <-chan bus.Event) {
	forwardBusFunc(func(ev bus.Event) { p.Send(busEventMsg{ev: ev}) }, sub)
}

// forwardBusFunc is forwardBus with the program send abstracted out
// for testing.
func forwardBusFunc(send func(bus.Event), sub <-chan bus.Event) {
	const flushInterval = 16 * time.Millisecond
	timer := time.NewTimer(flushInterval)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	var pending []bus.Event
	flush := func() {
		for _, ev := range coalesceDeltas(pending) {
			send(ev)
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
			if len(pending) == 0 {
				// First buffered event of a window: arm the flush
				// timer. It is guaranteed idle and drained here (it
				// either fired — emptying pending — or was never armed),
				// so Reset is race-free.
				timer.Reset(flushInterval)
			}
			pending = append(pending, ev)
		case <-timer.C:
			flush()
		}
	}
}

// coalesceDeltas merges runs of CONSECUTIVE streaming-delta events
// (EventAssistantDelta / EventReasoningDelta, string payloads) into a
// single event carrying the concatenated payload. Only adjacent
// same-type deltas merge — a non-delta event (or the other delta type)
// between two runs keeps them separate, so ordering is preserved
// exactly. Rewrites events in place and returns the shortened prefix.
func coalesceDeltas(events []bus.Event) []bus.Event {
	out := events[:0]
	for i := 0; i < len(events); {
		ev := events[i]
		j := i + 1
		if isStreamDelta(ev.Type) {
			if _, ok := ev.Payload.(string); ok {
				for j < len(events) && events[j].Type == ev.Type {
					if _, ok := events[j].Payload.(string); !ok {
						break
					}
					j++
				}
			}
		}
		if j > i+1 {
			var sb strings.Builder
			for k := i; k < j; k++ {
				sb.WriteString(events[k].Payload.(string))
			}
			ev = bus.Event{Type: ev.Type, Payload: sb.String()}
		}
		out = append(out, ev)
		i = j
	}
	return out
}

// isStreamDelta reports whether t is a per-token streaming delta that
// coalesceDeltas may merge.
func isStreamDelta(t bus.EventType) bool {
	return t == bus.EventAssistantDelta || t == bus.EventReasoningDelta
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
// confirmation that MCP/LSP and the isolation backend are spinning up;
// the same data is also reachable on demand via /info or the
// Ctrl-Space overlay.
func printStartupBanner(provider *llm.Provider, cfg *config.Config, writer *session.Writer) {
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
	if be := cfg.ResolveBackend(); be != config.BackendLocal {
		bits = append(bits, "backend: "+string(be))
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
// so replayed and live messages look identical. Tool calls are
// reconstructed from the persisted assistant tool_calls + their result
// rows (historyBlocks), so the resumed transcript shows the tools the
// session ran — they survive into scrollback and /find. Reasoning is
// not replayed: it is never persisted (re-derived each turn).
func replayHistory(history []llm.Message) {
	for _, b := range historyBlocks(history) {
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

// appendAuditEvent persists one auditable bus event into the events
// table. ToolCallEnd payloads get their bulky output fields truncated
// first — the full tool output is already persisted in the messages
// and tool_calls tables, so auditing it verbatim a third time grew
// enso.db ~3-4× the actual output volume.
func appendAuditEvent(writer *session.Writer, evt bus.Event) {
	typ := auditTypeName(evt.Type)
	if typ == "" {
		return
	}
	payload := sanitizePayload(evt.Payload)
	if evt.Type == bus.EventToolCallEnd {
		// Safe to mutate: sanitizePayload deep-copied the map, so the
		// shared bus payload other subscribers (the renderer!) receive
		// keeps the full strings.
		payload = truncateAuditedToolEnd(payload)
	}
	if err := writer.AppendEvent(typ, payload); err != nil {
		slog.Warn("session: append event", "type", typ, "err", err)
	}
}

// auditToolOutputCap bounds the result/display strings persisted to
// the events (audit) table for one ToolCallEnd.
const auditToolOutputCap = 256

// truncateAuditedToolEnd caps the result/display fields of an
// audit-path ToolCallEnd payload. Must only be called on the audit
// path's own sanitized copy — never on the shared bus event payload.
func truncateAuditedToolEnd(p any) any {
	m, ok := p.(map[string]any)
	if !ok {
		return p
	}
	for _, k := range []string{"result", "display"} {
		s, ok := m[k].(string)
		if !ok || len(s) <= auditToolOutputCap {
			continue
		}
		cut := auditToolOutputCap
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut-- // don't split a multi-byte rune
		}
		m[k] = s[:cut] + fmt.Sprintf("...[truncated, %d bytes total]", len(s))
	}
	return m
}
