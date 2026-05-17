// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sync"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/agents"
	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/hooks"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/lsp"
	"github.com/TaraTheStar/enso/internal/mcp"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/tools"
)

// RunAgent is the real worker-side AgentFunc: it reconstructs the agent
// core from the TaskSpec and maps the Channel onto the agent's concrete
// types — chan-string input, *bus.Bus output, a Channel-backed
// llm.ChatClient (host-proxied inference), and permission round-trips.
// It is the seam that makes the core hoistable; LocalBackend
// merely launches the process this runs in.
//
// Credential-scrub invariant: providers are built from
// spec.Providers (names/models only) with a Channel-backed client. The
// worker never holds an endpoint or API key and never dials a model;
// pool/rate gating stays host-side where the real providers live.
func RunAgent(ctx context.Context, spec backend.TaskSpec, ch backend.Channel) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cfg, err := decodeConfig(spec.ResolvedConfig)
	if err != nil {
		return fmt.Errorf("worker: decode resolved config: %w", err)
	}

	s := &seam{
		ch:        ch,
		inflight:  map[string]chan llm.Event{},
		pending:   map[string]chan permissions.Decision{},
		capWait:   map[string]chan wire.CapabilityGrant{},
		cancelAll: cancel,
	}

	providers, err := buildProviders(spec, s)
	if err != nil {
		return fmt.Errorf("worker: build providers: %w", err)
	}

	// The full agent recipe runs worker-side now (the agent core lives
	// here). This mirrors cmd/enso/run.go's construction so behavior is
	// identical; the host keeps only the real providers + rendering.
	denies := append([]string{}, cfg.Permissions.Deny...)
	if ignore, err := permissions.LoadIgnoreFile(filepath.Join(spec.Cwd, ".ensoignore")); err == nil {
		denies = append(denies, permissions.IgnoreToDenyPatterns(ignore)...)
	}
	checker := permissions.NewChecker(
		cfg.Permissions.Allow, cfg.Permissions.Ask, denies, cfg.Permissions.Mode,
	)
	if spec.Yolo {
		checker.SetYolo(true)
	}
	s.checker = checker // CtrlSetYolo toggles the real enforcing checker

	registry := tools.BuildDefault()
	agent.RegisterSpawn(registry)
	tools.RegisterSearch(registry, cfg.Search)

	mcpMgr := mcp.NewManager()
	if len(cfg.MCP) > 0 {
		mcpMgr.Start(context.Background(), cfg.MCP)
		mcpMgr.RegisterAll(registry)
	}
	defer mcpMgr.Close()

	lspMgr := lsp.NewManager(spec.Cwd, cfg.LSP)
	tools.RegisterLSP(registry, lspMgr)
	defer lspMgr.Close()

	var restrictedRoots []string
	if !cfg.Permissions.DisableFileConfinement {
		restrictedRoots = append([]string{spec.Cwd}, cfg.Permissions.AdditionalDirectories...)
	}

	provider := pickProvider(providers, spec.DefaultProvider)
	aspec, err := agents.Find(spec.Cwd, spec.AgentProfile)
	if err != nil {
		return fmt.Errorf("worker: agent profile: %w", err)
	}
	applied := agents.Apply(aspec, provider, registry)
	provider = applied.Provider
	registry = applied.Registry

	busInst := bus.New()

	var (
		store   *session.Store
		writer  *session.Writer
		resumed *session.State
	)
	if !spec.Ephemeral {
		store, err = session.Open()
		if err != nil {
			return fmt.Errorf("worker: open session store: %w", err)
		}
		defer store.Close()
		if spec.ResumeSessionID != "" {
			resumed, err = session.Load(store, spec.ResumeSessionID)
			if err != nil {
				return fmt.Errorf("worker: resume %s: %w", spec.ResumeSessionID, err)
			}
			writer, err = session.AttachWriter(store, spec.ResumeSessionID)
			if err != nil {
				return fmt.Errorf("worker: attach writer: %w", err)
			}
		} else if spec.SessionID != "" {
			// The host (which mints the id and shares the filesystem
			// under LocalBackend) already created the session row
			// before launch — owning the row host-side keeps it visible
			// for host audit/label/fork without racing the worker, and
			// mirrors the legacy in-process paths. The worker only
			// attaches a message-append writer to it.
			writer, err = session.AttachWriter(store, spec.SessionID)
			if err != nil {
				return fmt.Errorf("worker: attach session %s: %w", spec.SessionID, err)
			}
		} else {
			// No host-minted id (e.g. a caller that doesn't pre-create):
			// fall back to creating the row worker-side.
			writer, err = session.NewSessionWithID(store, "", provider.Model, provider.Name, spec.Cwd)
			if err != nil {
				return fmt.Errorf("worker: create session: %w", err)
			}
		}
	}

	maxTurns := spec.MaxTurns
	if applied.MaxTurns > 0 {
		maxTurns = applied.MaxTurns
	}

	acfg := agent.Config{
		Providers:             providers,
		DefaultProvider:       spec.DefaultProvider,
		Bus:                   busInst,
		Registry:              registry,
		Perms:                 checker,
		Writer:                writer,
		Cwd:                   spec.Cwd,
		MaxTurns:              maxTurns,
		GitAttribution:        cfg.Git.Attribution,
		GitAttributionName:    cfg.Git.AttributionName,
		ExtraSystemPrompt:     applied.PromptAppend,
		AdditionalDirectories: cfg.Permissions.AdditionalDirectories,
		RestrictedRoots:       restrictedRoots,
		Hooks:                 hooks.New(cfg.Hooks.OnFileEdit, cfg.Hooks.OnSessionEnd),
		WebFetchAllowHosts:    cfg.WebFetch.AllowHosts,
		PruneCfg:              cfg.Context.Resolve(),
		IsolationNote:         isolationNote(spec.Isolation),
	}
	if spec.Isolation.NetworkSealed {
		// Only a sealed worker brokers capabilities; on the unsealed
		// LocalBackend path it stays nil so tools behave exactly as
		// today (read host env / dial directly) per the interface
		// contract — no accidental default-deny of normal operation.
		acfg.Capabilities = s
	}
	if writer != nil {
		acfg.SessionID = writer.SessionID()
	}
	if resumed != nil {
		acfg.History = resumed.History
	}
	// Sandbox stays nil: LocalBackend is no-isolation by definition;
	// the container backend is a different Backend entirely.

	agt, err := agent.New(acfg)
	if err != nil {
		return fmt.Errorf("worker: construct agent: %w", err)
	}
	s.agt = agt

	// Seed the host with provider/model (and zeroed token counts)
	// before the first turn, so the status line is populated the
	// instant the worker is ready rather than only after activity.
	s.emitTelemetry()

	// Output + permission proxy: drain the bus, forward serializable
	// events as MsgEvent, intercept permission requests for round-trip.
	busSub := busInst.Subscribe(8192)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.forwardBus(busSub) }()

	// Input: a buffered chan string feeding agent.Run. Non-interactive
	// runs get the single prompt then a closed channel so the agent
	// exits when quiescent; interactive runs stay open for MsgInput.
	inputCh := make(chan string, 16)
	if spec.Prompt != "" {
		inputCh <- spec.Prompt
	}
	if !spec.Interactive {
		close(inputCh)
	} else {
		s.inputCh = inputCh
	}

	// Demux: the single reader of the Channel. Routes input, cancel,
	// shutdown, and correlated inference/permission responses. It is a
	// process-scoped daemon goroutine — it parks on a blocking Recv and
	// is reaped when the worker process exits (or unblocked by the host
	// closing the Channel). It must NOT gate teardown: joining it after
	// agt.Run returns would deadlock, since the host has no reason to
	// send anything more.
	go s.demux(ctx, inputCh, spec.Interactive)

	runErr := agt.Run(ctx, inputCh)

	cancel()
	busInst.Close() // ends forwardBus's range, bounded
	wg.Wait()
	return runErr
}

// session holds the per-task wiring shared by the demux, the bus
// forwarder, and the Channel-backed inference client.
type seam struct {
	ch        backend.Channel
	sendMu    sync.Mutex // Channel.Send must be serialized across producers
	agt       *agent.Agent
	checker   *permissions.Checker // the REAL enforcing checker (CtrlSetYolo)
	cancelAll context.CancelFunc

	inputCh   chan string // non-nil only for interactive specs
	inputOnce sync.Once

	mu       sync.Mutex
	inflight map[string]chan llm.Event            // inference corr -> stream
	pending  map[string]chan permissions.Decision // permission corr -> respond
	capWait  map[string]chan wire.CapabilityGrant // capability corr -> grant
	corrSeq  uint64

	// telemetry coalescing. emitTelemetry is called from the single
	// forwardBus goroutine (plus once from RunAgent before that
	// goroutine starts), so a small dedicated mutex protecting only the
	// last-sent snapshot is enough; we never want a no-op re-send to
	// take the hot s.mu.
	telemMu   sync.Mutex
	lastTelem wire.Telemetry
	telemSet  bool
}

// emitTelemetry snapshots the agent's token accounting + active
// provider and sends a MsgTelemetry, but only if the snapshot changed
// since the last send (coalesced — bus events fire far more often than
// any of these values move). The host fills context window + conn-state
// from its real provider; the worker, credential-scrubbed, reports
// neither.
func (s *seam) emitTelemetry() {
	if s.agt == nil {
		return
	}
	t := wire.Telemetry{
		Provider:  s.agt.ProviderName(),
		EstTokens: s.agt.EstimateTokens(),
		CumIn:     s.agt.CumulativeInputTokens(),
		CumOut:    s.agt.CumulativeOutputTokens(),
	}
	if p := s.agt.Provider(); p != nil {
		t.Model = p.Model
	}

	s.telemMu.Lock()
	if s.telemSet && t == s.lastTelem {
		s.telemMu.Unlock()
		return
	}
	s.lastTelem = t
	s.telemSet = true
	s.telemMu.Unlock()

	body, err := backend.NewBody(t)
	if err != nil {
		return
	}
	_ = s.send(backend.Envelope{Kind: backend.MsgTelemetry, Body: body})
}

func (s *seam) send(env backend.Envelope) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.ch.Send(env)
}

func (s *seam) nextCorr(prefix string) string {
	s.mu.Lock()
	s.corrSeq++
	n := s.corrSeq
	s.mu.Unlock()
	return fmt.Sprintf("%s-%d", prefix, n)
}

// demux is the sole consumer of ch.Recv(). It must never block on agent
// work; response delivery uses buffered channels.
func (s *seam) demux(ctx context.Context, inputCh chan string, interactive bool) {
	for {
		env, err := s.ch.Recv()
		if err != nil {
			// Host closed the Channel: stop the agent.
			s.cancelAll()
			return
		}
		switch env.Kind {
		case backend.MsgInput:
			if !interactive {
				continue // non-interactive input stream is closed
			}
			var in backend.InputBody
			if json.Unmarshal(env.Body, &in) == nil {
				select {
				case inputCh <- in.Text:
				case <-ctx.Done():
					return
				}
			}
		case backend.MsgCancel:
			if s.agt != nil {
				s.agt.Cancel()
			}
		case backend.MsgShutdown:
			if interactive {
				s.inputOnce.Do(func() { close(inputCh) })
			}
		case backend.MsgInferenceEvent, backend.MsgInferenceDone, backend.MsgInferenceError:
			s.routeInference(env)
		case backend.MsgPermissionDecision:
			s.routePermission(env)
		case backend.MsgCapabilityGrant:
			s.routeCapability(env)
		case backend.MsgControlRequest:
			// Own goroutine: force_compact does an LLM summary call
			// that round-trips through this very demux (host-proxied
			// inference) — running it inline would deadlock the reader.
			go s.serveControl(ctx, env)
		}
	}
}

// serveControl dispatches one host control RPC against the live agent
// and replies on the same Corr. The agent methods mirror exactly what
// the in-process TUI used to call directly, so behavior is identical;
// the only change is the call now crosses the seam.
func (s *seam) serveControl(ctx context.Context, env backend.Envelope) {
	var req wire.ControlRequest
	var resp wire.ControlResponse
	if err := json.Unmarshal(env.Body, &req); err != nil {
		resp.Error = fmt.Sprintf("decode control request: %v", err)
		s.sendControlResponse(env.Corr, resp)
		return
	}
	if s.agt == nil {
		resp.Error = "agent not ready"
		s.sendControlResponse(env.Corr, resp)
		return
	}

	switch req.Method {
	case wire.CtrlSetProvider:
		var a wire.ControlName
		_ = json.Unmarshal(req.Args, &a)
		if err := s.agt.SetProvider(a.Name); err != nil {
			resp.Error = err.Error()
		}
		s.emitTelemetry() // provider/model moved

	case wire.CtrlCompactPreview:
		p := s.agt.CompactPreview()
		resp.Result, _ = backend.NewBody(wire.CompactPreview{
			BeforeTokens:        p.BeforeTokens,
			EstAfterTokens:      p.EstAfterTokens,
			MessagesToSummarise: p.MessagesToSummarise,
			NothingToDo:         p.NothingToDo,
		})

	case wire.CtrlForceCompact:
		did, err := s.agt.ForceCompact(ctx)
		if err != nil {
			resp.Error = err.Error()
		} else {
			resp.Result, _ = backend.NewBody(wire.ForceCompactResult{Did: did})
		}
		s.emitTelemetry() // history shrank

	case wire.CtrlForcePrune:
		stubbed, before, after := s.agt.ForcePrune()
		resp.Result, _ = backend.NewBody(wire.ForcePruneResult{
			Stubbed: stubbed, Before: before, After: after,
		})
		s.emitTelemetry() // history shrank

	case wire.CtrlPrefixBreakdown:
		bd := s.agt.PrefixBreakdown()
		resp.Result, _ = backend.NewBody(wire.PrefixBreakdown{
			Total:        bd.Total,
			System:       bd.System,
			Pinned:       bd.Pinned,
			ToolActive:   bd.ToolActive,
			ToolStubbed:  bd.ToolStubbed,
			Conversation: bd.Conversation,
		})

	case wire.CtrlSetNextTurnTools:
		var a wire.ControlNames
		_ = json.Unmarshal(req.Args, &a)
		s.agt.SetNextTurnTools(a.Names)

	case wire.CtrlSetYolo:
		var a wire.ControlBool
		_ = json.Unmarshal(req.Args, &a)
		if s.checker != nil {
			s.checker.SetYolo(a.Value)
		}

	default:
		resp.Error = "unknown control method: " + req.Method
	}
	s.sendControlResponse(env.Corr, resp)
}

func (s *seam) sendControlResponse(corr string, resp wire.ControlResponse) {
	body, err := backend.NewBody(resp)
	if err != nil {
		return
	}
	_ = s.send(backend.Envelope{Kind: backend.MsgControlResponse, Corr: corr, Body: body})
}

// ---- host-proxied inference ----------------------------------------

type chanChatClient struct {
	s        *seam
	provider string // which configured provider this client speaks for
}

func (c chanChatClient) Chat(ctx context.Context, req llm.ChatRequest) (<-chan llm.Event, error) {
	corr := c.s.nextCorr("inf")
	stream := make(chan llm.Event, 32)

	c.s.mu.Lock()
	c.s.inflight[corr] = stream
	c.s.mu.Unlock()

	body, err := backend.NewBody(wire.InferenceRequest{
		Provider: c.provider,
		Request:  req,
	})
	if err != nil {
		c.s.dropInflight(corr)
		return nil, err
	}
	if err := c.s.send(backend.Envelope{
		Kind: backend.MsgInferenceRequest, Corr: corr, Body: body,
	}); err != nil {
		c.s.dropInflight(corr)
		return nil, fmt.Errorf("worker: send inference request: %w", err)
	}
	return stream, nil
}

func (s *seam) dropInflight(corr string) {
	s.mu.Lock()
	if ch, ok := s.inflight[corr]; ok {
		delete(s.inflight, corr)
		close(ch)
	}
	s.mu.Unlock()
}

func (s *seam) routeInference(env backend.Envelope) {
	s.mu.Lock()
	stream, ok := s.inflight[env.Corr]
	s.mu.Unlock()
	if !ok {
		return
	}
	switch env.Kind {
	case backend.MsgInferenceEvent:
		var w wire.LLMEvent
		if json.Unmarshal(env.Body, &w) != nil {
			return
		}
		stream <- w.ToLLM()
	case backend.MsgInferenceDone:
		s.dropInflight(env.Corr)
	case backend.MsgInferenceError:
		var eb backend.ErrorBody
		_ = json.Unmarshal(env.Body, &eb)
		stream <- llm.Event{Type: llm.EventError, Error: fmt.Errorf("%s", eb.Message)}
		s.dropInflight(env.Corr)
	}
}

// ---- tier-3 capability broker --------------------------------------

// RequestCapability satisfies tools.CapabilityRequester. It sends a
// MsgCapabilityRequest and blocks for the correlated grant (or ctx
// cancel / worker shutdown). The host policy default is deny, so an
// uncooperative or unconfigured host yields ok=false — exactly the
// fail-closed behavior a sealed worker wants.
func (s *seam) RequestCapability(ctx context.Context, capType, name, reason string) (string, int, bool) {
	corr := s.nextCorr("cap")
	reply := make(chan wire.CapabilityGrant, 1)
	s.mu.Lock()
	s.capWait[corr] = reply
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.capWait, corr)
		s.mu.Unlock()
	}()

	body, err := backend.NewBody(wire.CapabilityRequest{Type: capType, Name: name, Reason: reason})
	if err != nil {
		return "", 0, false
	}
	if err := s.send(backend.Envelope{Kind: backend.MsgCapabilityRequest, Corr: corr, Body: body}); err != nil {
		return "", 0, false
	}
	select {
	case g := <-reply:
		return g.Secret, g.TTLSeconds, g.Granted
	case <-ctx.Done():
		return "", 0, false
	}
}

func (s *seam) routeCapability(env backend.Envelope) {
	var g wire.CapabilityGrant
	_ = json.Unmarshal(env.Body, &g)
	s.mu.Lock()
	reply, ok := s.capWait[env.Corr]
	s.mu.Unlock()
	if ok {
		reply <- g // buffered (cap 1)
	}
}

// ---- bus output + permission proxy ---------------------------------

func (s *seam) forwardBus(sub <-chan bus.Event) {
	for evt := range sub {
		// Every bus event is a potential state change (a turn
		// completing moves token counts, a /model swap moves the
		// provider). Coalesced inside emitTelemetry, so this is cheap
		// even on the per-delta path.
		s.emitTelemetry()

		if evt.Type == bus.EventPermissionRequest {
			if pr, ok := evt.Payload.(*permissions.PromptRequest); ok {
				s.proxyPermission(pr)
			}
			continue
		}
		typ, payload, ok := evt.WireForm()
		if !ok {
			continue
		}
		body, err := backend.NewBody(backend.EventBody{Type: typ, Payload: payload})
		if err != nil {
			continue
		}
		_ = s.send(backend.Envelope{Kind: backend.MsgEvent, Body: body})
	}
}

func (s *seam) proxyPermission(pr *permissions.PromptRequest) {
	corr := s.nextCorr("perm")
	s.mu.Lock()
	s.pending[corr] = pr.Respond
	s.mu.Unlock()

	body, err := backend.NewBody(wire.PermissionRequest{
		Tool:      pr.ToolName,
		ArgString: pr.ArgString,
		Args:      pr.Args,
		Diff:      pr.Diff,
		AgentID:   pr.AgentID,
		AgentRole: pr.AgentRole,
	})
	if err != nil {
		s.deliverPermission(corr, permissions.Deny)
		return
	}
	if err := s.send(backend.Envelope{
		Kind: backend.MsgPermissionRequest, Corr: corr, Body: body,
	}); err != nil {
		s.deliverPermission(corr, permissions.Deny)
	}
}

func (s *seam) routePermission(env backend.Envelope) {
	var d wire.PermissionDecision
	_ = json.Unmarshal(env.Body, &d)
	decision := permissions.Deny
	if d.Decision == wire.PermAllow {
		decision = permissions.Allow
	}
	s.deliverPermission(env.Corr, decision)
}

func (s *seam) deliverPermission(corr string, d permissions.Decision) {
	s.mu.Lock()
	respond, ok := s.pending[corr]
	if ok {
		delete(s.pending, corr)
	}
	s.mu.Unlock()
	if ok {
		// Respond is buffered (cap 1) by the agent; non-blocking.
		select {
		case respond <- d:
		default:
		}
	}
}

// ---- construction helpers ------------------------------------------

// isolationNote turns the TaskSpec's IsolationSpec into the single
// honest sentence the # Environment prompt section shows. There is one
// filesystem namespace in every case now (the whole agent runs in the
// box), so the note describes the box's safety posture, not any
// path-translation rule — that rule no longer exists.
func isolationNote(is backend.IsolationSpec) string {
	if !is.NetworkSealed && is.Image == "" {
		// LocalBackend: the degenerate no-isolation Backend.
		return "none — this agent runs directly on the host; changes apply in place with no sandbox and no automatic rollback."
	}
	net := "network sealed (egress only via brokered, default-denied capabilities)"
	if !is.NetworkSealed {
		net = "network not sealed"
	}
	return fmt.Sprintf("container (image %s), %s. The entire agent runs inside the box on one filesystem. Workspace changes are not yet automatically rolled back.", is.Image, net)
}

func decodeConfig(raw json.RawMessage) (*config.Config, error) {
	cfg := &config.Config{}
	if len(raw) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(raw, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// pickProvider returns the named provider, or the alphabetically-first
// one when name is empty or unknown — mirroring agent.New's default
// selection so worker-side construction matches the host's.
func pickProvider(providers map[string]*llm.Provider, name string) *llm.Provider {
	if p, ok := providers[name]; ok {
		return p
	}
	first := ""
	for k := range providers {
		if first == "" || k < first {
			first = k
		}
	}
	return providers[first]
}

func buildProviders(spec backend.TaskSpec, s *seam) (map[string]*llm.Provider, error) {
	if len(spec.Providers) == 0 {
		return nil, fmt.Errorf("no providers in task spec")
	}
	out := make(map[string]*llm.Provider, len(spec.Providers))
	for _, p := range spec.Providers {
		out[p.Name] = &llm.Provider{
			Name:  p.Name,
			Model: p.Model,
			// Inference is host-proxied; the real pool/rate gating lives
			// host-side. A large local pool keeps Acquire a no-op. The
			// client is per-provider so the host knows which configured
			// endpoint to dial.
			Pool:             llm.NewPool(1 << 20),
			PoolName:         p.Pool,
			Client:           chanChatClient{s: s, provider: p.Name},
			IncludeProviders: true,
		}
	}
	return out, nil
}
