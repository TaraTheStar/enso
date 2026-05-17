// SPDX-License-Identifier: AGPL-3.0-or-later

// Package host is the host-side half of the Backend seam: the mirror of
// internal/backend/worker. It launches a Worker via a Backend and
// drives the Channel — serving host-proxied inference from the REAL
// providers (the host owns endpoints/keys/pools), republishing the
// worker's events onto a host *bus.Bus so the existing renderers work
// unchanged, and proxying permission requests through that same bus so
// the existing permission UIs work unchanged.
//
// The design goal is behavior-identical: a caller that used to do
// `agt, _ := agent.New(cfg); go agt.Run(ctx, inputCh)` and subscribe to
// a *bus.Bus instead does `Start(...)` and subscribes to the same bus.
package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/egress"
	"github.com/TaraTheStar/enso/internal/backend/local"
	"github.com/TaraTheStar/enso/internal/backend/podman"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// SelectBackend resolves the configured Backend and the IsolationSpec
// to stamp into TaskSpec. There is exactly one execution path: every
// caller (run, TUI, daemon) does `b, isol := SelectBackend(cfg)` then
// host.Start — no in-process branch. "Sandbox off" is the degenerate
// LocalBackend; "sandbox on" is PodmanBackend. Single host-side place
// this mapping is made so the call sites cannot drift.
func SelectBackend(cfg *config.Config) (backend.Backend, backend.IsolationSpec, []Option) {
	if cfg.ResolveBackend() != config.BackendPodman {
		return &local.Backend{}, backend.IsolationSpec{}, nil
	}
	sb := cfg.Bash.Sb
	img := sb.Image
	if img == "" {
		img = "docker.io/library/alpine:latest"
	}
	net := sb.Network
	if net == "" {
		net = "none" // sealed by default
	}
	b := &podman.Backend{
		Runtime:     cfg.Bash.Sandbox, // "auto"/"podman"/"docker"
		Image:       img,
		Network:     net,
		ExtraMounts: sb.ExtraMounts,
		Env:         sb.Env,
		UID:         sb.UID,
		OCIRuntime:  sb.OCIRuntime(),
		// MountSource (the throwaway workspace copy) is wired by the
		// run/TUI call site, which owns the overlay lifecycle and the
		// end-of-task commit/discard prompt.
	}
	isol := backend.IsolationSpec{
		NetworkSealed: net == "none",
		Image:         img,
		ExtraMounts:   sb.ExtraMounts,
		Runtime:       sb.OCIRuntime(),
	}

	// Tier-3 broker: only constructed when the operator configured
	// credentials or an egress allowlist. Otherwise the default-deny
	// broker stays in force (a sealed box with no grants at all).
	var opts []Option
	if len(sb.Credentials) > 0 || len(sb.Egress) > 0 {
		broker := &AllowlistBroker{
			Creds:  sb.Credentials,
			Egress: map[string]bool{},
		}
		for _, e := range sb.Egress {
			broker.Egress[e] = true
		}
		if len(sb.Egress) > 0 {
			pr := egress.New()
			if err := pr.Start(); err == nil {
				broker.Proxy = pr
				b.EgressProxy = pr.ProxyURL() // box's only route out
			}
		}
		opts = append(opts, WithBroker(broker))
	}
	return b, isol, opts
}

// Session is a running worker driven over the Channel. Its lifetime
// mirrors a top-level agent.Run: Start brings it up, Wait blocks until
// it finishes, Close tears it down.
type Session struct {
	worker    backend.Worker
	ch        backend.Channel
	providers map[string]*llm.Provider
	bus       *bus.Bus

	sendMu sync.Mutex // Channel.Send serialized across producers

	doneOnce sync.Once
	done     chan error

	inputClosed sync.Once

	telemMu sync.Mutex
	telem   wire.Telemetry

	ctlMu      sync.Mutex
	ctlSeq     uint64
	ctlPending map[string]chan wire.ControlResponse

	broker CapabilityBroker
}

// CapabilityBroker authorizes a network-sealed worker's tier-3
// requests for a scoped secret or a one-off egress allowance. The
// contract is fail-closed: a nil broker (the default) denies
// everything, so capability access is opt-in by construction.
type CapabilityBroker interface {
	Authorize(ctx context.Context, req wire.CapabilityRequest) wire.CapabilityGrant
}

// Option customizes a Session at Start. Variadic + optional so the
// existing call sites (run, TUI, daemon, tests) are unaffected.
type Option func(*Session)

// WithBroker installs the tier-3 capability broker. Without it, every
// capability request is denied.
func WithBroker(b CapabilityBroker) Option {
	return func(s *Session) { s.broker = b }
}

// denyBroker is the default policy: deny all, with an auditable reason.
type denyBroker struct{}

func (denyBroker) Authorize(context.Context, wire.CapabilityRequest) wire.CapabilityGrant {
	return wire.CapabilityGrant{Granted: false, Reason: "no capability broker configured (default deny)"}
}

// AllowlistBroker is the concrete tier-3 policy: a static, explicit
// allowlist. Credentials map a logical name to its secret value;
// egress maps an allowed "host:port" to true. Anything not listed is
// denied (default-deny preserved). On a granted egress request the
// host:port is added live to Proxy so the sealed container's only
// route out opens exactly for that target and nothing else.
type AllowlistBroker struct {
	Creds  map[string]string // CapCredential name -> secret
	Egress map[string]bool   // CapEgress "host:port" -> allowed
	Proxy  *egress.Proxy     // optional; live-allowed on egress grants
	TTL    int               // advisory seconds on grants (0 = unset)
}

func (a *AllowlistBroker) Authorize(_ context.Context, req wire.CapabilityRequest) wire.CapabilityGrant {
	switch req.Type {
	case wire.CapCredential:
		if v, ok := a.Creds[req.Name]; ok {
			return wire.CapabilityGrant{Granted: true, Secret: v, TTLSeconds: a.TTL}
		}
		return wire.CapabilityGrant{Reason: "credential " + req.Name + " not on allowlist"}
	case wire.CapEgress:
		if a.Egress[req.Name] {
			if a.Proxy != nil {
				a.Proxy.Allow(req.Name)
			}
			return wire.CapabilityGrant{Granted: true, TTLSeconds: a.TTL}
		}
		return wire.CapabilityGrant{Reason: "egress to " + req.Name + " not on allowlist"}
	}
	return wire.CapabilityGrant{Reason: "unknown capability type " + req.Type}
}

// Telemetry is the host-exposed snapshot of the agent state the TUI
// status line / overlay used to read directly off *agent.Agent. It is
// the seam's replacement for those direct reads: worker-sourced fields
// (token accounting + the active provider the agent selected) arrive
// over MsgTelemetry; ContextWindow and ConnState are filled here from
// the REAL provider, which only the host has (the worker is
// credential-scrubbed — no configured window, no live transport).
type Telemetry struct {
	Provider      string
	Model         string
	EstTokens     int
	CumIn         int64
	CumOut        int64
	ContextWindow int
	ConnState     llm.ConnState
}

// Telemetry returns the current merged snapshot. Safe to call from any
// goroutine; cheap enough for a status-line refresh ticker.
func (s *Session) Telemetry() Telemetry {
	s.telemMu.Lock()
	t := s.telem
	s.telemMu.Unlock()

	out := Telemetry{
		Provider:  t.Provider,
		Model:     t.Model,
		EstTokens: t.EstTokens,
		CumIn:     t.CumIn,
		CumOut:    t.CumOut,
	}
	// Augment with what only the host has: the real provider's
	// configured context window and its live transport conn-state.
	if p := s.providers[t.Provider]; p != nil {
		out.ContextWindow = p.ContextWindow
		if out.Model == "" {
			out.Model = p.Model
		}
		if r, ok := p.Client.(llm.ConnStateReporter); ok {
			out.ConnState = r.LLMConnState()
		}
	}
	return out
}

// ProviderCatalog projects the host's REAL provider set down to the
// non-secret []backend.ProviderInfo that crosses the seam in
// TaskSpec.Providers — names, models and pool only, never endpoint or
// key. It is the single host-side place this projection is made so
// run, the TUI and the daemon cannot drift on what the worker sees.
//
// The ordering is sorted purely for a deterministic, diff-stable
// catalog (and stable prompt rendering). It deliberately does NOT
// drive default-provider selection: every call site resolves the
// default name host-side and passes it explicitly in
// TaskSpec.DefaultProvider, and the worker's pickProvider always finds
// it by that exact name — the alphabetical fallback there is
// unreachable on these paths, so catalog order can never cause a
// host/worker default mismatch.
func ProviderCatalog(providers map[string]*llm.Provider) []backend.ProviderInfo {
	out := make([]backend.ProviderInfo, 0, len(providers))
	for name, p := range providers {
		out = append(out, backend.ProviderInfo{Name: name, Model: p.Model, Pool: p.PoolName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Start launches the worker, performs the handshake (send MsgTaskSpec,
// await MsgWorkerReady), and begins serving the seam. It returns once
// the worker's agent core is constructed and ready — mirroring
// agent.New returning a ready agent.
func Start(
	ctx context.Context,
	b backend.Backend,
	spec backend.TaskSpec,
	providers map[string]*llm.Provider,
	busInst *bus.Bus,
	opts ...Option,
) (*Session, error) {
	w, err := b.Start(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("host: start worker: %w", err)
	}
	s := &Session{
		worker:     w,
		ch:         w.Channel(),
		providers:  providers,
		bus:        busInst,
		done:       make(chan error, 1),
		ctlPending: map[string]chan wire.ControlResponse{},
		broker:     denyBroker{},
	}
	for _, o := range opts {
		o(s)
	}
	if s.broker == nil {
		s.broker = denyBroker{}
	}
	// Seed from the spec so Telemetry() is populated in the brief
	// window between MsgWorkerReady and the worker's first
	// MsgTelemetry. The worker's snapshot supersedes this as soon as it
	// arrives (it carries the agent's actual provider selection).
	s.telem.Provider = spec.DefaultProvider
	if p := providers[spec.DefaultProvider]; p != nil {
		s.telem.Model = p.Model
	}

	specBody, err := backend.NewBody(spec)
	if err != nil {
		_ = w.Teardown(context.Background())
		return nil, err
	}
	if err := s.send(backend.Envelope{Kind: backend.MsgTaskSpec, Body: specBody}); err != nil {
		_ = w.Teardown(context.Background())
		return nil, fmt.Errorf("host: send task spec: %w", err)
	}

	ready := make(chan error, 1)
	go s.loop(ctx, ready)

	select {
	case err := <-ready:
		if err != nil {
			// A Backend may know WHY its box never came up (e.g. podman
			// printed an OCI-runtime error to stderr). Surface it so the
			// user gets something actionable instead of a bare EOF.
			if d, ok := w.(interface{ StartupDiagnostic() string }); ok {
				if msg := d.StartupDiagnostic(); msg != "" {
					err = fmt.Errorf("%w\n\n%s", err, msg)
				}
			}
			_ = w.Teardown(context.Background())
			return nil, err
		}
	case <-ctx.Done():
		_ = w.Teardown(context.Background())
		return nil, ctx.Err()
	}
	return s, nil
}

func (s *Session) send(env backend.Envelope) error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	return s.ch.Send(env)
}

// Submit feeds a user message to the worker (interactive specs).
func (s *Session) Submit(text string) error {
	body, err := backend.NewBody(backend.InputBody{Text: text})
	if err != nil {
		return err
	}
	return s.send(backend.Envelope{Kind: backend.MsgInput, Body: body})
}

// Cancel aborts the worker's in-flight turn (Ctrl-C equivalent).
func (s *Session) Cancel() { _ = s.send(backend.Envelope{Kind: backend.MsgCancel}) }

// CloseInput tells an interactive worker no more input is coming so it
// winds down when quiescent.
func (s *Session) CloseInput() {
	s.inputClosed.Do(func() {
		_ = s.send(backend.Envelope{Kind: backend.MsgShutdown})
	})
}

// Wait blocks until the worker finishes (MsgWorkerDone), errors
// (MsgWorkerError), or the Channel drops. Returns the run's terminal
// error (nil on clean completion).
func (s *Session) Wait() error { return <-s.done }

// Close tears the worker down. Idempotent; safe after Wait.
func (s *Session) Close() error { return s.worker.Teardown(context.Background()) }

func (s *Session) finish(err error) {
	s.doneOnce.Do(func() { s.done <- err; close(s.done) })
}

// loop is the sole reader of the Channel. It never blocks on agent
// work: inference and permission service run in their own goroutines.
func (s *Session) loop(ctx context.Context, ready chan<- error) {
	readyOnce := sync.Once{}
	signalReady := func(err error) { readyOnce.Do(func() { ready <- err }) }

	for {
		env, err := s.ch.Recv()
		if err != nil {
			// Channel dropped. If the worker never reported a terminal
			// status this is an abnormal exit.
			signalReady(fmt.Errorf("host: worker channel closed before ready: %w", err))
			s.finish(nil)
			return
		}
		switch env.Kind {
		case backend.MsgWorkerReady:
			signalReady(nil)

		case backend.MsgEvent:
			var eb backend.EventBody
			if json.Unmarshal(env.Body, &eb) == nil {
				if ev, ok := bus.FromWire(eb.Type, eb.Payload); ok {
					s.bus.Publish(ev)
				}
			}

		case backend.MsgTelemetry:
			var tw wire.Telemetry
			if json.Unmarshal(env.Body, &tw) == nil {
				s.telemMu.Lock()
				s.telem = tw
				s.telemMu.Unlock()
			}

		case backend.MsgCapabilityRequest:
			go s.serveCapability(ctx, env.Corr, env.Body)

		case backend.MsgControlResponse:
			s.routeControl(env)

		case backend.MsgInferenceRequest:
			go s.serveInference(ctx, env.Corr, env.Body)

		case backend.MsgPermissionRequest:
			go s.servePermission(ctx, env.Corr, env.Body)

		case backend.MsgWorkerError:
			var e backend.ErrorBody
			_ = json.Unmarshal(env.Body, &e)
			signalReady(fmt.Errorf("worker: %s", e.Message))
			s.finish(fmt.Errorf("worker: %s", e.Message))
			return

		case backend.MsgWorkerDone:
			signalReady(nil)
			s.finish(nil)
			return
		}
	}
}

// control issues one synchronous control RPC to the worker and blocks
// for the correlated response (or ctx cancel). args may be nil. The
// returned error is the worker-side method error (ControlResponse.Error)
// or a transport failure; callers map it to their concrete signature.
func (s *Session) control(ctx context.Context, method string, args any) (json.RawMessage, error) {
	var rawArgs json.RawMessage
	if args != nil {
		b, err := backend.NewBody(args)
		if err != nil {
			return nil, err
		}
		rawArgs = b
	}
	body, err := backend.NewBody(wire.ControlRequest{Method: method, Args: rawArgs})
	if err != nil {
		return nil, err
	}

	s.ctlMu.Lock()
	s.ctlSeq++
	corr := fmt.Sprintf("ctl-%d", s.ctlSeq)
	resp := make(chan wire.ControlResponse, 1)
	s.ctlPending[corr] = resp
	s.ctlMu.Unlock()

	defer func() {
		s.ctlMu.Lock()
		delete(s.ctlPending, corr)
		s.ctlMu.Unlock()
	}()

	if err := s.send(backend.Envelope{Kind: backend.MsgControlRequest, Corr: corr, Body: body}); err != nil {
		return nil, fmt.Errorf("host: send control %s: %w", method, err)
	}
	select {
	case r := <-resp:
		if r.Error != "" {
			return nil, errors.New(r.Error)
		}
		return r.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, errors.New("host: worker exited before control response")
	}
}

// ---- typed control facade -----------------------------------------
//
// Thin typed wrappers over control(); they mirror the *agent.Agent
// methods the TUI used to call in-process. Results are the agent-free
// wire mirrors — the caller (the bubble adapter) converts to the agent
// types and builds provider context from the REAL providers it already
// holds, so no agent/instructions dependency leaks into this package.

// SetProvider switches the worker agent's active provider.
func (s *Session) SetProvider(ctx context.Context, name string) error {
	_, err := s.control(ctx, wire.CtrlSetProvider, wire.ControlName{Name: name})
	return err
}

// CompactPreview returns the worker agent's compaction preview.
func (s *Session) CompactPreview(ctx context.Context) (wire.CompactPreview, error) {
	raw, err := s.control(ctx, wire.CtrlCompactPreview, nil)
	if err != nil {
		return wire.CompactPreview{}, err
	}
	var p wire.CompactPreview
	err = json.Unmarshal(raw, &p)
	return p, err
}

// ForceCompact runs worker-side context compaction (an LLM summary
// call, host-proxied). Returns whether it did anything.
func (s *Session) ForceCompact(ctx context.Context) (bool, error) {
	raw, err := s.control(ctx, wire.CtrlForceCompact, nil)
	if err != nil {
		return false, err
	}
	var r wire.ForceCompactResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return false, err
	}
	return r.Did, nil
}

// ForcePrune runs worker-side stale-tool pruning. Returns
// (stubbed, beforeTokens, afterTokens), matching agent.ForcePrune.
func (s *Session) ForcePrune(ctx context.Context) (int, int, int, error) {
	raw, err := s.control(ctx, wire.CtrlForcePrune, nil)
	if err != nil {
		return 0, 0, 0, err
	}
	var r wire.ForcePruneResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return 0, 0, 0, err
	}
	return r.Stubbed, r.Before, r.After, nil
}

// PrefixBreakdown returns the worker agent's per-category token split.
func (s *Session) PrefixBreakdown(ctx context.Context) (wire.PrefixBreakdown, error) {
	raw, err := s.control(ctx, wire.CtrlPrefixBreakdown, nil)
	if err != nil {
		return wire.PrefixBreakdown{}, err
	}
	var bd wire.PrefixBreakdown
	err = json.Unmarshal(raw, &bd)
	return bd, err
}

// SetNextTurnTools restricts the worker agent's next turn to names
// (skills tool-gating). Fire-and-forget semantics but RPC'd so it is
// ordered before the subsequent MsgInput.
func (s *Session) SetNextTurnTools(ctx context.Context, names []string) error {
	_, err := s.control(ctx, wire.CtrlSetNextTurnTools, wire.ControlNames{Names: names})
	return err
}

// SetYolo toggles the worker's REAL enforcing permission checker. The
// TUI also mirrors the flag onto a host-side display checker so /info
// and the overlay reflect it without a round-trip.
func (s *Session) SetYolo(ctx context.Context, on bool) error {
	_, err := s.control(ctx, wire.CtrlSetYolo, wire.ControlBool{Value: on})
	return err
}

func (s *Session) routeControl(env backend.Envelope) {
	var r wire.ControlResponse
	_ = json.Unmarshal(env.Body, &r)
	s.ctlMu.Lock()
	ch, ok := s.ctlPending[env.Corr]
	s.ctlMu.Unlock()
	if ok {
		ch <- r // buffered (cap 1); non-blocking
	}
}

// serveCapability runs one tier-3 broker decision in its own goroutine
// (the broker may block on user confirmation) and replies on the same
// Corr. Fail-closed: any decode failure denies.
func (s *Session) serveCapability(ctx context.Context, corr string, body json.RawMessage) {
	var req wire.CapabilityRequest
	grant := wire.CapabilityGrant{Granted: false, Reason: "malformed capability request"}
	if json.Unmarshal(body, &req) == nil {
		grant = s.broker.Authorize(ctx, req)
	}
	b, err := backend.NewBody(grant)
	if err != nil {
		return
	}
	_ = s.send(backend.Envelope{Kind: backend.MsgCapabilityGrant, Corr: corr, Body: b})
}

// serveInference runs one host-proxied model call. The host owns the
// real provider (endpoint/key) and the real Pool — rate/concurrency
// gating is centralized here, across every worker and sub-agent.
func (s *Session) serveInference(ctx context.Context, corr string, body json.RawMessage) {
	var ir wire.InferenceRequest
	if err := json.Unmarshal(body, &ir); err != nil {
		s.sendInferenceError(corr, fmt.Sprintf("decode request: %v", err))
		return
	}
	p := s.providers[ir.Provider]
	if p == nil {
		s.sendInferenceError(corr, fmt.Sprintf("unknown provider %q", ir.Provider))
		return
	}

	release, err := p.Pool.Acquire(ctx)
	if err != nil {
		s.sendInferenceError(corr, fmt.Sprintf("pool: %v", err))
		return
	}
	defer release()

	evCh, err := p.Client.Chat(ctx, ir.Request)
	if err != nil {
		s.sendInferenceError(corr, err.Error())
		return
	}
	for ev := range evCh {
		b, err := backend.NewBody(wire.FromLLM(ev))
		if err != nil {
			continue
		}
		if err := s.send(backend.Envelope{
			Kind: backend.MsgInferenceEvent, Corr: corr, Body: b,
		}); err != nil {
			return
		}
	}
	_ = s.send(backend.Envelope{Kind: backend.MsgInferenceDone, Corr: corr})
}

func (s *Session) sendInferenceError(corr, msg string) {
	b, err := backend.NewBody(backend.ErrorBody{Message: msg})
	if err != nil {
		return
	}
	_ = s.send(backend.Envelope{Kind: backend.MsgInferenceError, Corr: corr, Body: b})
}

// servePermission reconstructs a real *permissions.PromptRequest,
// publishes it on the host bus exactly as an in-process agent would,
// and relays the user's decision back. The existing permission UIs
// (run.go auto-deny, TUI modal, daemon proxy) handle it unchanged.
func (s *Session) servePermission(ctx context.Context, corr string, body json.RawMessage) {
	var pw wire.PermissionRequest
	if err := json.Unmarshal(body, &pw); err != nil {
		s.sendDecision(corr, permissions.Deny)
		return
	}
	respond := make(chan permissions.Decision, 1)
	s.bus.Publish(bus.Event{
		Type: bus.EventPermissionRequest,
		Payload: &permissions.PromptRequest{
			ToolName:  pw.Tool,
			ArgString: pw.ArgString,
			Args:      pw.Args,
			Diff:      pw.Diff,
			AgentID:   pw.AgentID,
			AgentRole: pw.AgentRole,
			Respond:   respond,
		},
	})
	select {
	case d := <-respond:
		s.sendDecision(corr, d)
	case <-ctx.Done():
		s.sendDecision(corr, permissions.Deny)
	}
}

func (s *Session) sendDecision(corr string, d permissions.Decision) {
	val := wire.PermDeny
	if d == permissions.Allow {
		val = wire.PermAllow
	}
	b, err := backend.NewBody(wire.PermissionDecision{Decision: val})
	if err != nil {
		return
	}
	_ = s.send(backend.Envelope{Kind: backend.MsgPermissionDecision, Corr: corr, Body: b})
}
