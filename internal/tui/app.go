// SPDX-License-Identifier: AGPL-3.0-or-later

package tui

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

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
	"github.com/TaraTheStar/enso/internal/workflow"
)

// Options configures a TUI run.
type Options struct {
	Yolo      bool
	Session   string // session id to resume; empty = new session
	Ephemeral bool   // if true, do not persist
	MaxTurns  int
	Config    string // optional explicit config file (-c)
	Agent     string // declarative agent profile name; empty = default
}

// Run launches the enso TUI application.
func Run(opts Options) error {
	// pendingSwitch, when set inside the event loop (Ctrl-R → Enter on a
	// session), tells the post-loop tail to syscall.Exec into the same
	// binary with --session <id> substituted. Cleaner than trying to swap
	// out the live agent / writer / chat / etc. mid-flight.
	var pendingSwitch string

	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("get cwd: %w", err)
	}

	// Apply the bundled muted-pastel palette first, then layer any user
	// theme.toml overrides on top so the chosen palette takes effect on
	// first draw. Errors are logged but non-fatal — a typo in theme.toml
	// shouldn't block the TUI.
	ApplyDefaultPalette()
	if path, err := DefaultThemePath(); err == nil {
		if err := LoadTheme(path); err != nil {
			slog.Warn("theme load", "path", path, "err", err)
		}
	}

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
	// `provider` below is the initial active one — used for status bar
	// init, session writer naming, and chat display construction. After
	// agent.New the agent owns the full Providers map and a /model
	// switch updates the active one.
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

	// Per-agent transcripts — populated by spawn_agent / workflow.runRole
	// post-completion, read by the agents-pane click-to-expand overlay.
	transcripts := tools.NewTranscripts()

	// MCP servers: connect (10s per server), register their tools.
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

	// Session setup.
	var (
		store        *session.Store
		writer       *session.Writer
		resumed      *session.State
		sessionTitle string
		resumeNotice string
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
			sessionTitle = shortID(opts.Session)
			if resumed.Interrupted {
				resumeNotice = "(resumed: previous turn was interrupted; tool calls auto-recovered)"
				_ = writer.MarkInterrupted(false)
			}
		} else {
			writer, err = session.NewSession(store, provider.Model, provider.Name, cwd)
			if err != nil {
				return fmt.Errorf("create session: %w", err)
			}
			sessionTitle = shortID(writer.SessionID())
		}
	} else {
		sessionTitle = "ephemeral"
	}

	maxTurns := opts.MaxTurns
	if applied.MaxTurns > 0 {
		maxTurns = applied.MaxTurns
	}

	// Hooks instance — Warn is rebound below once the chat view exists
	// so timeouts/template errors surface as a yellow chat line. Until
	// then any failure (very unlikely before agent.Run starts) goes to
	// slog via the default Warn.
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
	// See cmd/enso/run.go for why Sandbox is conditionally assigned:
	// avoid the typed-nil-into-interface trap.
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

	layout := NewLayout()
	chatDisp := NewChatDisplay(layout.Chat(), provider.Model)

	app := tview.NewApplication()

	// Replay resumed history into the chat view, or paint the splash
	// (logo + recent sessions) on a fresh launch so the user knows what
	// they can resume without opening the Ctrl-R picker. splashIDs
	// holds the displayed session ids in order — Alt-1..Alt-5 below
	// resume the matching one.
	var splashIDs []string
	if resumed != nil {
		chatDisp.ReplayHistory(resumed.History, provider.Model)
		if resumeNotice != "" {
			fmt.Fprintf(layout.Chat(), "[yellow]%s[-]\n\n", resumeNotice)
		}
	} else {
		splashIDs = RenderSplash(layout.Chat(), store)
	}

	// AssistantDelta fires once per token. Rendering each one through
	// QueueUpdateDraw individually saturates tview's redraw queue on
	// fast streams (>~100 t/s) — old code dropped events when the
	// 4096-deep subscriber channel filled. We coalesce arrivals into
	// 16ms (~60fps) windows: every event is still rendered in order,
	// but the UI redraws once per batch rather than once per token.
	// Buffer bumped to 8192 as belt-and-braces during reasoning bursts
	// before the first tick fires.
	chatEvents := busInst.Subscribe(8192)
	go func() {
		const flushInterval = 16 * time.Millisecond
		ticker := time.NewTicker(flushInterval)
		defer ticker.Stop()
		var pending []bus.Event
		flush := func() {
			if len(pending) == 0 {
				return
			}
			batch := pending
			pending = nil
			app.QueueUpdateDraw(func() {
				for _, ev := range batch {
					chatDisp.Render(ev)
				}
			})
		}
		for {
			select {
			case ev, ok := <-chatEvents:
				if !ok {
					flush()
					return
				}
				pending = append(pending, ev)
			case <-ticker.C:
				flush()
			}
		}
	}()

	// Session inspector (right sidebar). Tracks subagent spawns/ends in
	// the same bus subscriber as before, but otherwise reads its data
	// from the live agent / lsp / mcp managers each refresh.
	sessionStart := time.Now()
	sessionIDForSidebar := ""
	if writer != nil {
		sessionIDForSidebar = writer.SessionID()
	}
	sidebar := NewSidebar(
		layout.SidebarView(),
		agt,
		sessionIDForSidebar,
		cwd,
		sessionStart,
		lspMgr,
		mcpMgr,
	)
	// Surface the persisted label (auto-derived on prior turns or
	// /rename'd) when resuming. Fresh sessions stay unlabelled until
	// the first user message lands and AppendMessage's auto-label
	// fires; the host pulls that back via the bus listener below.
	if resumed != nil && resumed.Info.Label != "" {
		sidebar.SetLabel(resumed.Info.Label)
	}
	sidebar.Refresh()

	agentsEvents := busInst.Subscribe(32)
	go func() {
		for evt := range agentsEvents {
			ev := evt
			if ev.Type != bus.EventAgentStart && ev.Type != bus.EventAgentEnd {
				continue
			}
			app.QueueUpdateDraw(func() {
				sidebar.HandleEvent(ev)
				sidebar.Refresh()
			})
		}
	}()

	// Permission modal subscriber. Serialised: handle one request at a time.
	permEvents := busInst.Subscribe(8)
	go func() {
		for evt := range permEvents {
			if evt.Type != bus.EventPermissionRequest {
				continue
			}
			req, ok := evt.Payload.(*permissions.PromptRequest)
			if !ok {
				continue
			}
			pinned := req
			onRemember := func() {
				pattern := permissions.DerivePattern(pinned.ToolName, pinned.Args)
				if err := checker.AddAllow(pattern); err != nil {
					fmt.Fprintf(layout.Chat(), "[red]remember %s: %v[-]\n\n", pattern, err)
					return
				}
				path := config.ProjectLocalPath(cwd)
				if err := config.AppendAllow(path, pattern); err != nil {
					fmt.Fprintf(layout.Chat(), "[yellow]remembered %s in this session, but couldn't persist: %v[-]\n\n", pattern, err)
					return
				}
				fmt.Fprintf(layout.Chat(), "[teal]remembered %s → %s[-]\n\n", pattern, path)
			}
			// Turn-scoped grant: same pattern derivation as Remember, but
			// not persisted to disk and reset on the next user message.
			// The agent loop calls Perms.ResetTurnAllows before every
			// EventUserMessage, so this lives only as long as the user's
			// current request fans out.
			onAllowTurn := func() {
				pattern := permissions.DerivePattern(pinned.ToolName, pinned.Args)
				if err := checker.AddTurnAllow(pattern); err != nil {
					fmt.Fprintf(layout.Chat(), "[red]allow-turn %s: %v[-]\n\n", pattern, err)
					return
				}
				fmt.Fprintf(layout.Chat(), "[teal]allowing %s for this turn[-]\n\n", pattern)
			}
			app.QueueUpdateDraw(func() {
				ShowPermissionModal(app, layout.Pages(), layout.Input(), onRemember, onAllowTurn, pinned)
			})
		}
	}()

	// Buffered comfortably so submitters never block in normal use. Each
	// submit also spawns a goroutine that selects on ctx.Done(), so even if
	// this fills it can't deadlock the UI.
	inputCh := make(chan string, 64)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Git working-tree watcher: an async worker fetches `git status`
	// off the tview goroutine (so a slow repo can't stall the UI) and
	// repaints the sidebar via QueueUpdateDraw when results land. Kick
	// once at startup to populate the initial state; further refreshes
	// fire from EventToolCallEnd in the activity subscriber below.
	sidebar.SetGitRefreshCallback(func() {
		app.QueueUpdateDraw(sidebar.Refresh)
	})
	go sidebar.RunGitWatcher(ctx)
	sidebar.TriggerGitRefresh()

	// Surface hook timeouts and template errors as a yellow chat line.
	// Non-zero exit codes from the user's command are silently swallowed
	// (option (c) — see internal/hooks). Reassigned here, before Run
	// starts, so no goroutine ever reads a half-initialised callback.
	hooksInst.Warn = func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		slog.Warn(msg)
		app.QueueUpdateDraw(func() {
			fmt.Fprintf(layout.Chat(), "[yellow]%s[-]\n\n", msg)
			layout.Chat().ScrollToEnd()
		})
	}

	go func() {
		if err := agt.Run(ctx, inputCh); err != nil && err != context.Canceled {
			busInst.Publish(bus.Event{Type: bus.EventError, Payload: err})
		}
	}()

	mode := "PROMPT"
	if opts.Yolo {
		mode = "AUTO"
	}
	activity := NewActivity()
	statusTpl, statusErr := compileStatusLine(cfg.UI.StatusLine)
	if statusErr != nil {
		fmt.Fprintf(layout.Chat(), "[yellow]ui.status_line: %v — using default[-]\n\n", statusErr)
	}

	// Sidebar starts open; Ctrl-A toggles. Declared up here so the
	// status-line renderer (`mkRight` below) can close over it — the
	// default template hides the tokens segment when the sidebar is
	// open to avoid duplicating the sidebar's token bar.
	sidebarVisible := true

	// Live streaming rate: streamStartNanos is the wall-clock time the
	// current turn began emitting deltas (0 = not streaming);
	// streamChars is the running total of delta-payload bytes for that
	// turn. Both are touched from the bus subscriber goroutine and read
	// from the tview render goroutine, so atomics keep them honest.
	var streamStartNanos atomic.Int64
	var streamChars atomic.Int64
	streamRate := func() int {
		start := streamStartNanos.Load()
		if start == 0 {
			return 0
		}
		elapsed := time.Since(time.Unix(0, start))
		// Suppress wild numbers from the first few deltas of a turn.
		if elapsed < 200*time.Millisecond {
			return 0
		}
		tokens := float64(streamChars.Load()) / 4.0
		rate := tokens / elapsed.Seconds()
		if rate < 1 {
			return 0
		}
		return int(rate)
	}

	mkRight := func() string {
		// Read the active provider live: /model switches mid-session
		// must be reflected on the next refresh.
		p := agt.Provider()
		ctx := statusContext{
			Provider:       p.Name,
			Model:          p.Model,
			Session:        sessionTitle,
			Mode:           mode,
			Activity:       activityLabel(activity.State()),
			Tokens:         agt.EstimateTokens(),
			Window:         agt.ContextWindow(),
			TokensFmt:      fmtTokens(agt.EstimateTokens(), agt.ContextWindow()),
			TokensPerSec:   streamRate(),
			SidebarVisible: sidebarVisible,
			ConnState:      fmtConnState(p.Client),
		}
		return renderStatusLine(statusTpl, ctx)
	}
	refreshStatus := func() {
		layout.SetStatus(fmt.Sprintf("%s · %s", mode, activity.Render()), mkRight())
	}
	refreshStatus()

	// Streaming-rate meter: a dedicated subscriber tracks delta payload
	// sizes against the first delta's timestamp. Reset on turn boundaries
	// so the rate reflects the current turn only.
	rateEvents := busInst.Subscribe(4096)
	go func() {
		for evt := range rateEvents {
			switch evt.Type {
			case bus.EventAssistantDelta, bus.EventReasoningDelta:
				if s, ok := evt.Payload.(string); ok {
					streamStartNanos.CompareAndSwap(0, time.Now().UnixNano())
					streamChars.Add(int64(len(s)))
				}
			case bus.EventAgentIdle, bus.EventCancelled, bus.EventError:
				// Reset the t/s counter when the pipeline goes idle —
				// resetting it on every per-completion AssistantDone
				// would also zero it between intermediate tool-call
				// turns, hiding the rate during the rest of the run.
				streamStartNanos.Store(0)
				streamChars.Store(0)
			}
		}
	}()

	// Status-bar tick: at 100ms cadence, advance the activity spinner
	// (when busy) and refresh the bar so the streaming-rate (t/s)
	// segment stays current. Strictly no-op when nothing's happening
	// — terminals invalidate any active text selection on app output,
	// and a periodic timer-driven redraw kills mouse drag-to-copy.
	// Sidebar refreshes piggyback on bus events instead.
	//
	// Exception: the LLM connection-state probe flips state at idle
	// when the endpoint comes back. We can't redraw on every tick or
	// we lose drag-select, so we sample the current segment each tick
	// and only redraw when it actually changed.
	go func() {
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		var lastConn string
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				busy := activity.IsBusy()
				if busy {
					activity.Tick()
				}
				connNow := fmtConnState(agt.Provider().Client)
				connChanged := connNow != lastConn
				lastConn = connNow
				if busy || streamStartNanos.Load() != 0 || connChanged {
					app.QueueUpdateDraw(refreshStatus)
				}
			}
		}
	}()

	// Tool-call elapsed-time ticker: when a tool call has been running
	// past the live-badge threshold, repaint the chat once a second so
	// the "running · 12s" segment advances. Idle sessions take zero
	// redraws here — the gate inside HasLiveTimerBlock keeps cost to a
	// per-second slice walk and nothing more.
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if chatDisp.HasLiveTimerBlock() {
					app.QueueUpdateDraw(chatDisp.Redraw)
				}
			}
		}
	}()

	// Slash command registry + builtins.
	slashReg := slash.NewRegistry()
	sCtx := &slashContext{
		app:         app,
		pages:       layout.Pages(),
		chat:        layout.Chat(),
		chatDisp:    chatDisp,
		agt:         agt,
		checker:     checker,
		registry:    registry,
		store:       store,
		writer:      writer,
		cwd:         cwd,
		activeAgent: opts.Agent,
		setSessionLabel: func(label string) (string, error) {
			slug := session.SlugifyLabel(label)
			if writer != nil {
				if err := writer.SetLabel(slug); err != nil {
					return "", err
				}
			}
			sidebar.SetLabel(slug)
			app.QueueUpdateDraw(sidebar.Refresh)
			return slug, nil
		},
		runDeps: workflow.RunDeps{
			Providers:          providers,
			DefaultProvider:    defaultName,
			Bus:                busInst,
			Registry:           registry,
			Perms:              checker,
			Cwd:                cwd,
			MaxTurns:           opts.MaxTurns,
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
		},
		stop: func() {
			cancel()
			app.Stop()
		},
		setMode: func(m string) {
			mode = m
			activity.Set(ActivityReady, "")
			refreshStatus()
		},
		switchSession: func(id string) {
			pendingSwitch = id
			cancel()
			app.Stop()
		},
	}
	// submit is defined below; assign it to sCtx after the closure is in
	// scope so /init and skill commands can both inject synthetic input.
	registerBuiltins(slashReg, sCtx)

	// submit pushes a message onto the agent's input channel without ever
	// blocking the UI thread. Spawning a goroutine for the send is essential:
	// if the agent is stuck (slow tool, hung server, awaiting a permission
	// response) and the channel fills, a synchronous send freezes the
	// tcell event loop and Ctrl-D becomes undeliverable.
	submit := func(text string) {
		go func() {
			select {
			case inputCh <- text:
			case <-ctx.Done():
			}
		}()
	}

	// Custom skills from ~/.enso/skills and ./.enso/skills. The submitter
	// pushes the rendered template into the agent's input channel as if the
	// user had typed it.
	skillSubmit := func(text string, allowedTools []string) {
		// allowed-tools restricts the next turn to a subset of the
		// registry. Set BEFORE submit so the agent sees the restriction
		// when it picks up the new user message.
		agt.SetNextTurnTools(allowedTools)
		submit(text)
	}
	sCtx.submit = skillSubmit
	if skills, err := slash.LoadSkills(cwd); err != nil {
		fmt.Fprintf(layout.Chat(), "[yellow]skills load: %v[-]\n\n", err)
	} else {
		for _, sk := range skills {
			sk.SetSubmitter(skillSubmit)
			slashReg.Register(sk)
		}
	}

	// Session-inspector sidebar visible by default; Ctrl-A toggles for
	// full-width chat. `sidebarVisible` is declared above so the
	// status-line renderer can close over it. Drawn after the layout
	// is wired but before SetRoot runs so the first frame already
	// includes it.
	layout.ShowSidebar(true)
	var handler *InputHandler
	handler = NewInputHandler(
		layout.Input(),
		func(text string) {
			// User has engaged: stop intercepting Alt-1..Alt-5 for
			// splash-resume so those keystrokes go back to whatever
			// the input field wants them for. Also re-engage chat
			// auto-follow — if they scrolled up earlier, submitting
			// a new message obviously means they want to see what
			// happens next at the bottom.
			splashIDs = nil
			chatDisp.SetFollowBottom(true)
			if name, args, ok := slash.Parse(text); ok {
				cmd := slashReg.Get(name)
				if cmd == nil {
					fmt.Fprintf(layout.Chat(), "[red]unknown command: /%s (try /help)[-]\n\n", name)
					layout.Chat().ScrollToEnd()
					return
				}
				go func() {
					if err := cmd.Run(ctx, args); err != nil {
						app.QueueUpdateDraw(func() {
							fmt.Fprintf(layout.Chat(), "[red]/%s: %v[-]\n\n", name, err)
							layout.Chat().ScrollToEnd()
						})
					}
				}()
				return
			}
			activity.Set(ActivitySubmitting, "")
			refreshStatus()
			submit(text)
		},
		func() {
			// Cancel any in-flight turn so the agent goroutine returns
			// promptly; otherwise app.Stop() can race the still-running
			// turn and leave the UI feeling unresponsive.
			agt.Cancel()
			cancel()
			app.Stop()
		},
	)

	// Vim-mode wiring: editor_mode = "vim" in [ui] enables it. The mode
	// label is surfaced as a prefix on the status bar's left half so
	// users can tell at a glance whether typing inserts text.
	if strings.EqualFold(cfg.UI.EditorMode, "vim") {
		baseMode := mode
		handler.EnableVim(true, func(vm string) {
			mode = baseMode + " · " + vm
			refreshStatus()
		})
	}

	// @-trigger: open the file picker. On selection, insert the path
	// at the cursor with a trailing space so the next character the user
	// types lands cleanly outside the path token.
	handler.SetOnAtTrigger(func() {
		ShowFilePickerOverlay(app, layout.Pages(), layout.Input(), cwd, cfg.Permissions.AdditionalDirectories, ignorePatterns, func(path string) {
			handler.InsertAtCursor(path + " ")
		})
	})

	// Audit log: persist a curated subset of bus events into the
	// session's `events` table so diagnostic replay is possible. Skips
	// per-token deltas (would bloat the table) and any payload that
	// can't be JSON-marshalled (PermissionRequest carries a chan).
	if writer != nil {
		auditCh := busInst.Subscribe(64)
		go func() {
			for evt := range auditCh {
				typ := auditTypeName(evt.Type)
				if typ == "" {
					continue
				}
				payload := sanitizePayload(evt.Payload)
				if err := writer.AppendEvent(typ, payload); err != nil {
					// "database is closed" is a benign shutdown
					// race: agent emitted a late event (typically
					// Cancelled) after the deferred store.Close fired.
					// The row really is lost, but warning about it
					// every shutdown adds noise without insight.
					if !strings.Contains(err.Error(), "database is closed") {
						slog.Warn("session: append event", "type", typ, "err", err)
					}
				}
			}
		}()
	}

	// Activity-state subscriber: drives the animated indicator off bus
	// events. SetBusy on the input handler also lives here so Enter is
	// re-enabled exactly when the agent is done. Refresh only fires when
	// updateActivityFromEvent reports a real transition (returns false
	// on the 50th reasoning delta in a row).
	//
	// The sidebar is also refreshed here on token-changing events,
	// which is what previously happened on a 1s timer — moving it onto
	// bus events means we don't redraw the screen while the user is
	// trying to drag-select chat text in idle.
	doneEvents := busInst.Subscribe(64)
	go func() {
		for evt := range doneEvents {
			ev := evt
			changed := updateActivityFromEvent(activity, ev)
			refreshSidebar := false
			switch ev.Type {
			case bus.EventAgentIdle, bus.EventCancelled, bus.EventError:
				// Pipeline-level "done". Activity is cleared by the
				// updateActivityFromEvent call above; the only thing
				// pinned to this branch now is the sidebar refresh.
				refreshSidebar = true
				// AppendMessage's auto-label may have fired during this
				// turn; pull the current label so a freshly-derived slug
				// shows up on the next paint. One DB read per turn end —
				// negligible cost.
				if writer != nil {
					if label, err := writer.Label(); err == nil {
						sidebar.SetLabel(label)
					}
				}
			case bus.EventCompacted:
				// Token count just dropped — refresh both the status
				// bar's tokens segment and the sidebar's bar.
				changed = true
				refreshSidebar = true
			case bus.EventToolCallEnd:
				// Tool output may have added tokens to the context.
				refreshSidebar = true
				// A tool call may have touched files (write/edit/bash).
				// The watcher debounces, so a flurry of edits in one
				// turn collapses to one git invocation.
				sidebar.TriggerGitRefresh()
			}
			if changed || refreshSidebar {
				app.QueueUpdateDraw(func() {
					if changed {
						refreshStatus()
					}
					if refreshSidebar {
						sidebar.Refresh()
					}
				})
			}
		}
	}()

	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		// Application-level Ctrl-key shortcuts. Match by Key first so
		// terminals that report ModCtrl alongside KeyCtrlR (kitty
		// extended keyboard protocol etc.) don't fall through to the
		// generic modifier early-return below — that bug previously
		// silently swallowed Ctrl-R on those terminals.
		switch event.Key() {
		case tcell.KeyCtrlC:
			// tview's Application.Run intercepts Ctrl-C BEFORE the
			// focused primitive's InputCapture, calling a.Stop() if the
			// app-level capture returns the event unchanged. So Ctrl-C
			// has to be handled here, not in InputHandler — otherwise
			// pressing it during a stuck turn exits the app instead of
			// cancelling. Always returning nil keeps tview from stopping.
			//
			// Gating on activity.IsBusy() rather than handler.IsBusy()
			// matters: handler.busy was being cleared by EventAssistantDone
			// after every intermediate turn (model emits a tool call →
			// AssistantDone → busy=false → tools run → next turn …),
			// leaving Ctrl-C silently no-op'd between turns. Activity is
			// driven off the full event stream and stays busy across the
			// whole pipeline.
			if activity.IsBusy() {
				agt.Cancel()
			}
			return nil
		case tcell.KeyCtrlA:
			sidebarVisible = !sidebarVisible
			layout.ShowSidebar(sidebarVisible)
			// Default template gates the tokens segment on sidebar
			// state — repaint immediately so collapsing/expanding
			// the sidebar doesn't leave a stale status bar until the
			// next event tick.
			refreshStatus()
			return nil
		case tcell.KeyCtrlT:
			chatDisp.ToggleThinking()
			return nil
		case tcell.KeyCtrlR:
			ShowSessionsOverlay(app, layout.Pages(), layout.Input(), layout.Chat(), store, func(id string) {
				pendingSwitch = id
				cancel()
				app.Stop()
			})
			return nil
		case tcell.KeyCtrlF:
			ShowFindOverlay(app, layout.Pages(), layout.Input(), chatDisp, "", false)
			return nil
		case tcell.KeyPgUp:
			// Page-scroll the chat without stealing focus. Disables
			// auto-follow so streaming content doesn't yank the
			// viewport back down while the user reads.
			chatDisp.SetFollowBottom(false)
			row, col := layout.Chat().GetScrollOffset()
			layout.Chat().ScrollTo(row-10, col)
			return nil
		case tcell.KeyPgDn:
			row, col := layout.Chat().GetScrollOffset()
			layout.Chat().ScrollTo(row+10, col)
			return nil
		case tcell.KeyEnd:
			// Jump to bottom and re-engage auto-follow so subsequent
			// streaming chunks scroll naturally again.
			chatDisp.SetFollowBottom(true)
			return nil
		case tcell.KeyHome:
			chatDisp.SetFollowBottom(false)
			layout.Chat().ScrollToBeginning()
			return nil
		}
		// Alt-1..Alt-5 on the splash: resume the matching session.
		// splashIDs is empty after the first user message lands (we
		// clear it below) so this stops firing as soon as the user
		// engages with the session — Alt-N then becomes free again
		// for whatever the input field wants to do with it.
		if event.Modifiers()&tcell.ModAlt != 0 && event.Rune() >= '1' && event.Rune() <= '9' {
			idx := int(event.Rune() - '1')
			if idx >= 0 && idx < len(splashIDs) {
				pendingSwitch = splashIDs[idx]
				cancel()
				app.Stop()
				return nil
			}
		}
		return event
	})

	if err := layout.SetRoot(app); err != nil {
		return fmt.Errorf("tui: %w", err)
	}

	if pendingSwitch != "" {
		return execIntoSession(pendingSwitch)
	}
	// Drop empty sessions: opening enso and closing without typing
	// anything otherwise leaves a 0-message row in the picker forever.
	// Discard cascades through messages/events/tool_calls via FKs, so
	// no orphan rows survive. Print the resume hint only for sessions
	// we actually kept.
	if writer != nil {
		if !writer.HasMessages() {
			if err := writer.Discard(); err != nil {
				slog.Warn("session: discard empty", "err", err)
			}
		} else {
			sid := writer.SessionID()
			fmt.Fprintf(os.Stdout, "\nsession %s\n  resume: enso --session %s   (or: enso --continue)\n",
				sid, sid)
		}
	}
	return nil
}

// auditTypeName returns the persisted name for the given bus event, or
// empty if the event is excluded from the audit log. Streaming deltas
// (AssistantDelta, ReasoningDelta, ToolCallProgress) are excluded —
// per-token rows would dominate the table; the final messages and the
// tool_calls rows already capture the durable state.
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

// sanitizePayload coerces non-JSON-marshallable fields (errors, channels)
// into safe representations so AppendEvent doesn't choke on them.
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

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// fmtTokens renders a "used/window" segment for the status bar with a
// colour cue based on how close the conversation is to compaction:
//
//	<50%  default
//	≥50%  yellow (compaction will trigger ~60%)
//	≥80%  red    (close to the hard limit)
//
// Window=0 (unconfigured) just shows the used count.
func fmtTokens(used, window int) string {
	if window <= 0 {
		return compactTokenCount(used)
	}
	pct := float64(used) / float64(window)
	open, close := "", ""
	switch {
	case pct >= 0.80:
		open, close = "[red]", "[-]"
	case pct >= 0.50:
		open, close = "[yellow]", "[-]"
	}
	return fmt.Sprintf("%s%s/%s%s", open, compactTokenCount(used), compactTokenCount(window), close)
}

func compactTokenCount(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
