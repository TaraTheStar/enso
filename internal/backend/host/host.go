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
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/TaraTheStar/azoth/llm"
	"github.com/TaraTheStar/enso/internal/backend"
	"github.com/TaraTheStar/enso/internal/backend/egress"
	"github.com/TaraTheStar/enso/internal/backend/lima"
	"github.com/TaraTheStar/enso/internal/backend/local"
	"github.com/TaraTheStar/enso/internal/backend/podman"
	"github.com/TaraTheStar/enso/internal/backend/wire"
	"github.com/TaraTheStar/enso/internal/backend/workspace"
	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/provider"
	"github.com/TaraTheStar/enso/internal/session"
)

// SelectBackend resolves the configured Backend and the IsolationSpec
// to stamp into TaskSpec. There is exactly one execution path: every
// caller (run, TUI, daemon) does `b, isol := SelectBackend(cfg)` then
// host.Start — no in-process branch. "Sandbox off" is the degenerate
// LocalBackend; "sandbox on" is PodmanBackend. Single host-side place
// this mapping is made so the call sites cannot drift.
// yolo (the launch-time --yolo flag) widens egress: the box stays
// structurally sealed and host-mediated, but the egress proxy runs
// allow-all (no default-deny gate). It keys off the launch flag, not the
// runtime /yolo toggle — a `--network none` container / sealed VM can't
// be re-plumbed mid-task, so runtime /yolo still only affects permission
// prompts. The unsealed LocalBackend ignores yolo (already open).
//
// interactive (true only for the attended TUI; false for `enso run` /
// any headless path) decides whether a denied egress prompts the
// operator or fails closed with a reason. A sealed box with a TTY thus
// becomes usable for research/build without a pre-enumerated allowlist;
// a headless one stays strictly default-deny.
func SelectBackend(cfg *config.Config, yolo, interactive bool) (backend.Backend, backend.IsolationSpec, []Option) {
	// Failing safe to local is fine; doing it silently is not. An
	// unrecognized [backend] type means the isolation the user asked
	// for is ABSENT — flag it visibly (stderr, not just the log file)
	// before falling through to the LocalBackend default.
	if bad := cfg.UnrecognizedBackendType(); bad != "" {
		msg := fmt.Sprintf("config error: unrecognized [backend] type %q — falling back to \"local\" (NO isolation: changes apply directly to the host, no rollback). Valid values: local, podman, lima.", bad)
		slog.Error("config: " + msg)
		fmt.Fprintln(os.Stderr, "enso: "+msg)
	}
	switch cfg.ResolveBackend() {
	case config.BackendLima:
		lb := &lima.Backend{
			Template:    cfg.Backend.Lima.Template,
			Init:        cfg.Backend.Lima.Init,
			CPUs:        cfg.Backend.Lima.CPUs,
			Memory:      cfg.Backend.Lima.Memory,
			Disk:        cfg.Backend.Lima.Disk,
			ExtraMounts: cfg.Backend.Lima.ExtraMounts,
			Sealed:      true, // guest egress firewalled default-deny at launch
			// MountSource (the per-project stable workspace copy) is
			// wired by the run/TUI call site, which owns the overlay
			// lifecycle and the end-of-task commit/discard prompt.
		}
		// Genuinely sealed now (not just by inference being host-proxied):
		// lima.Backend.Sealed firewalls guest OUTPUT default-deny, and the
		// same static broker + egress proxy podman uses are wired in so a
		// configured allowlist opens the box's only route out. NetworkSealed
		// is honest because the seal is enforced, not merely asserted.
		isol := backend.IsolationSpec{NetworkSealed: true, Kind: "vm", EgressUnrestricted: yolo}
		opts := egressBrokerOpts(cfg.Backend.Egress, yolo, interactive, func(u string) { lb.EgressProxy = u })
		return lb, isol, opts
	case config.BackendPodman:
		// fall through to the podman construction below
	default:
		return &local.Backend{}, backend.IsolationSpec{}, nil
	}
	sb := cfg.Backend.Podman
	img := sb.Image
	if img == "" {
		img = "docker.io/library/alpine:latest"
	}
	net := sb.Network
	if net == "" {
		net = "none" // sealed by default
	}
	b := &podman.Backend{
		Runtime:     cfg.PodmanRuntime(), // [backend] runtime; "" → "auto"
		Image:       img,
		Network:     net,
		ExtraMounts: sb.ExtraMounts,
		Env:         sb.Env,
		Init:        sb.Init,
		UID:         sb.UID,
		OCIRuntime:  sb.OCIRuntime(),
		// MountSource (the throwaway workspace copy) is wired by the
		// run/TUI call site, which owns the overlay lifecycle and the
		// end-of-task commit/discard prompt.
	}
	isol := backend.IsolationSpec{
		NetworkSealed:      net == "none",
		Image:              img,
		Kind:               "container", // worker FS is isolated → remote session persistence
		ExtraMounts:        sb.ExtraMounts,
		Runtime:            sb.OCIRuntime(),
		EgressUnrestricted: yolo && net == "none",
	}

	opts := egressBrokerOpts(cfg.Backend.Egress, yolo, interactive, func(u string) { b.EgressProxy = u })
	return b, isol, opts
}

// egressBrokerOpts builds the tier-3 capability broker the SAME way for
// every sealed backend (podman, lima) so the two cannot drift: a
// static allowlist broker, only constructed when the operator
// configured credentials or an egress allowlist (otherwise the
// default-deny policy stays in force — a sealed box with no grants at
// all). When an egress allowlist is configured it also starts the host
// allowlist proxy and hands its URL back via setProxy so the backend
// can make it the box's only route out. Nil result → no broker.
//
// yolo (launch-time --yolo) widens this to allow-all: the proxy is
// started unconditionally and put in allow-all mode (the box's only
// route out, but ungated), and the broker grants every egress. The box
// stays structurally sealed and all traffic still passes through this
// host proxy — only the default-deny gate is lifted. Config credentials
// remain explicit even under yolo.
//
// interactive wraps the static broker in an InteractiveBroker so a
// denied egress prompts the operator instead of hard-failing — and
// because a granted egress needs a route out, the proxy is started even
// with no pre-configured allowlist (it just begins empty/default-deny
// and grows by interactive grant). A non-interactive run keeps the old
// behavior exactly: static broker only, proxy only when something
// static can use it.
func egressBrokerOpts(eg config.EgressConfig, yolo, interactive bool, setProxy func(string)) []Option {
	hasStatic := yolo || len(eg.Credentials) > 0 || len(eg.Allow) > 0
	if !hasStatic && !interactive {
		return nil // nothing can ever grant → denyBroker default (== pre-feature)
	}

	static := &AllowlistBroker{
		Creds:          eg.Credentials,
		Egress:         map[string]bool{},
		AllowAllEgress: yolo,
	}
	for _, e := range eg.Allow {
		static.Egress[e] = true
	}

	// Start + inject the proxy whenever egress can be granted at all:
	// statically (yolo / configured list) OR interactively (a grant with
	// no proxy is a no-op — the box would stay sealed with no route out).
	if yolo || len(eg.Allow) > 0 || interactive {
		pr := egress.New()
		if err := pr.Start(); err == nil {
			if yolo {
				pr.AllowAll()
			}
			// Seed the proxy's CONFIGURED allowlist with the config-file
			// entries up front. AllowConfigured is the only path that
			// confers the SSRF-denylist opt-out, and this is its only
			// call site: the operator typed these names, so a configured
			// host that resolves to a private address (e.g. a LAN-hosted
			// SearXNG) gets the documented loopback/LAN opt-out
			// regardless of yolo/interactive. Runtime grants (broker /
			// interactive) go through pr.Allow instead and stay subject
			// to the denylist — a worker-chosen host:port must never
			// waive private-address protection.
			for _, e := range eg.Allow {
				pr.AllowConfigured(e)
			}
			static.Proxy = pr
			setProxy(pr.ProxyURL()) // box's only route out
		}
	}

	if !interactive {
		return []Option{WithBroker(static)}
	}
	ib := NewInteractiveBroker(static, true)
	if static.Proxy != nil {
		static.Proxy.SetDecider(ib) // covers bash + proxied web_* uniformly
	}
	return []Option{WithBroker(ib)}
}

// Session is a running worker driven over the Channel. Its lifetime
// mirrors a top-level agent.Run: Start brings it up, Wait blocks until
// it finishes, Close tears it down.
type Session struct {
	worker    backend.Worker
	ch        backend.Channel
	providers map[string]*provider.Provider
	bus       *bus.Bus

	// chWriter is the single owner of Channel.Send: producers enqueue,
	// control-plane envelopes (Cancel, permission/control responses)
	// overtake bulk inference/event traffic. Replaces the old sendMu +
	// blocking ch.Send per producer, which let a stream flood head-of-line
	// block a Cancel (findings #2 & #3).
	chWriter *backend.QueueWriter

	doneOnce sync.Once
	done     chan error

	inputClosed sync.Once

	// shutdownRequested flips when WE asked the worker to go away
	// (CloseInput's MsgShutdown or Close's Teardown). The loop uses it
	// to tell an expected channel EOF apart from an abnormal one: a
	// drop with no terminal message and no requested shutdown is a
	// worker crash (OOM-kill, VM death, panic) and must surface as an
	// error from Wait(), not a clean nil.
	shutdownRequested atomic.Bool

	telemMu sync.Mutex
	telem   wire.Telemetry

	ctlMu      sync.Mutex
	ctlSeq     uint64
	ctlPending map[string]chan wire.ControlResponse

	// infMu guards infCancel — the per-corr cancellers for in-flight
	// host-proxied inferences. The worker sends MsgInferenceCancel when
	// agent.Cancel() fires, and the demux loop looks the corr up here to
	// abort the real provider call. Without this the HTTP stream runs to
	// completion and the worker-side agent stays blocked.
	infMu     sync.Mutex
	infCancel map[string]context.CancelFunc

	broker CapabilityBroker

	// writer, when set (WithWriter), is the HOST-side session writer an
	// isolated worker's persist envelopes are applied to. nil for the
	// local backend (the worker writes the shared host DB itself) and
	// ephemeral sessions.
	writer *session.Writer

	// Per-turn /rewind checkpointing for an ISOLATED backend (WithCheckpointer).
	// The worker's MsgCheckpoint signals snapshotting ckptMergedDir (the
	// overlay's `merged` dir, which lives host-side) into the session's
	// checkpoint store at the just-applied top-level user turn. All nil/zero
	// for the local backend (the worker snapshots itself) and when no overlay
	// is in use. ckptMu serializes snapshots so concurrent turns can't race
	// the store/disk; the copy itself runs off the Channel loop goroutine.
	ckptStore     *session.Store
	ckptMergedDir string
	ckptRetain    int
	ckptMu        sync.Mutex
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

// WithWriter installs the host-side session writer that an ISOLATED
// worker's MsgPersistMessage/MsgPersistToolCall envelopes are applied
// to (the worker can't reach the host DB itself). Not set for the local
// backend (worker writes the shared DB directly) or ephemeral runs.
func WithWriter(w *session.Writer) Option {
	return func(s *Session) { s.writer = w }
}

// WithCheckpointer enables host-side per-turn /rewind checkpointing for
// an ISOLATED backend whose agent works in an overlay. mergedDir is the
// overlay's `merged` dir (workspace.Overlay.Copy) on the host; on each
// MsgCheckpoint the host snapshots it into the session's checkpoint store
// and prunes to `retain` most recent. Not set for the local backend (the
// worker snapshots the shared project tree itself) or when no overlay is
// in use (then there is no host-visible agent FS to snapshot — only
// conversation rewind works). Requires WithWriter (the same session).
func WithCheckpointer(store *session.Store, mergedDir string, retain int) Option {
	return func(s *Session) {
		s.ckptStore = store
		s.ckptMergedDir = mergedDir
		s.ckptRetain = retain
	}
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

	// AllowAllEgress (--yolo) grants every CapEgress request regardless of
	// the Egress allowlist. Credentials are unaffected — they remain
	// explicit, since "all network" does not mean "all secrets". The
	// matching allow-all proxy is what actually carries the traffic.
	AllowAllEgress bool
}

func (a *AllowlistBroker) Authorize(_ context.Context, req wire.CapabilityRequest) wire.CapabilityGrant {
	switch req.Type {
	case wire.CapCredential:
		if v, ok := a.Creds[req.Name]; ok {
			return wire.CapabilityGrant{Granted: true, Secret: v, TTLSeconds: a.TTL}
		}
		return wire.CapabilityGrant{Reason: "credential " + req.Name + " not on allowlist"}
	case wire.CapEgress:
		if a.AllowAllEgress || a.Egress[req.Name] {
			if a.Proxy != nil {
				// Runtime grant: opens the proxy gate for this target but
				// does NOT exempt it from the SSRF denylist — req.Name is
				// worker-supplied, so under yolo (grant-everything) this
				// must not become a loopback/metadata relay. Only
				// operator config entries (AllowConfigured) opt out.
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
	// CompactionBudget is the input-token target at which proactive
	// compaction fires (the ctx gauge's amber/marker threshold). Derived
	// host-side from the real provider, same formula the worker's agent
	// uses, so the gauge marker matches the actual trigger.
	CompactionBudget int
	ConnState        llm.ConnState
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
		out.CompactionBudget = p.InputBudget()
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
func ProviderCatalog(providers map[string]*provider.Provider) []backend.ProviderInfo {
	out := make([]backend.ProviderInfo, 0, len(providers))
	for name, p := range providers {
		out = append(out, backend.ProviderInfo{Name: name, Model: p.Model, Pool: p.PoolName})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// RecordWorkerAttach stamps execution provenance for one worker attach
// onto the session: the sessions.backend column (latest backend — the
// cheap "where did this run" answer for pickers/inspection) plus one
// WorkerAttached event row (the per-epoch audit record, so a session
// resumed under a different backend keeps an honest history of where
// each span of rows executed). Call it right after host.Start succeeds,
// from every attach site (run, run --workflow, TUI). Best-effort: a nil
// writer (ephemeral / local fallback) is a no-op, and a write failure
// must not kill the run — it logs like the other persist failures.
func RecordWorkerAttach(w *session.Writer, b backend.Backend, isol backend.IsolationSpec, taskID string) {
	if w == nil || b == nil {
		return
	}
	if err := w.SetBackend(b.Name()); err != nil {
		slog.Warn("host: record session backend failed", "backend", b.Name(), "err", err)
	}
	payload := map[string]any{
		"backend": b.Name(),
		"task_id": taskID,
	}
	if isol.Kind != "" {
		payload["kind"] = isol.Kind
	}
	if isol.Image != "" {
		payload["image"] = isol.Image
	}
	if err := w.AppendEvent("WorkerAttached", payload); err != nil {
		slog.Warn("host: record worker attach failed", "backend", b.Name(), "err", err)
	}
}

// Start launches the worker, performs the handshake (send MsgTaskSpec,
// await MsgWorkerReady), and begins serving the seam. It returns once
// the worker's agent core is constructed and ready — mirroring
// agent.New returning a ready agent.
func Start(
	ctx context.Context,
	b backend.Backend,
	spec backend.TaskSpec,
	providers map[string]*provider.Provider,
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
		infCancel:  map[string]context.CancelFunc{},
		broker:     denyBroker{},
	}
	for _, o := range opts {
		o(s)
	}
	if s.broker == nil {
		s.broker = denyBroker{}
	}
	// Single writer goroutine owns Channel.Send from here on; every
	// s.send() below (including the MsgTaskSpec handshake) enqueues onto it.
	// loop() closes it when the session ends.
	s.chWriter = backend.NewQueueWriter(s.ch)
	// The interactive broker prompts over the host bus, which does not
	// exist at SelectBackend time — bind it now, before the worker can
	// issue any request (the handshake below gates that).
	if bb, ok := s.broker.(interface{ BindBus(*bus.Bus) }); ok {
		bb.BindBus(busInst)
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
		_ = s.chWriter.Close()
		_ = w.Teardown(context.Background())
		return nil, err
	}
	if err := s.send(backend.Envelope{Kind: backend.MsgTaskSpec, Body: specBody}); err != nil {
		_ = s.chWriter.Close()
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
	return s.chWriter.Send(env)
}

// Submit feeds a user message to the worker (interactive specs).
// images carries any user-attached image bytes (resolved host-side from
// `@path` mentions); nil/empty for a plain text turn.
func (s *Session) Submit(text string, images []backend.InputImage) error {
	body, err := backend.NewBody(backend.InputBody{Text: text, Images: images})
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
		s.shutdownRequested.Store(true)
		_ = s.send(backend.Envelope{Kind: backend.MsgShutdown})
	})
}

// Wait blocks until the worker finishes (MsgWorkerDone), errors
// (MsgWorkerError), or the Channel drops. Returns the run's terminal
// error (nil on clean completion).
func (s *Session) Wait() error { return <-s.done }

// Close tears the worker down. Idempotent; safe after Wait.
func (s *Session) Close() error {
	// Flag BEFORE Teardown closes the Channel so the loop's Recv error
	// reads as the requested shutdown it is, not a worker crash.
	s.shutdownRequested.Store(true)
	return s.worker.Teardown(context.Background())
}

func (s *Session) finish(err error) {
	s.doneOnce.Do(func() { s.done <- err; close(s.done) })
}

// captureCheckpoint snapshots the isolated overlay's `merged` dir for the
// given top-level user-turn seq, off the worker's MsgCheckpoint signal.
// It runs in its own goroutine so the sole Channel reader never blocks on
// a tree copy (the snapshot has the whole inference round-trip to finish
// before the agent's first tool edit reaches `merged`), and is serialized
// by ckptMu so back-to-back turns don't race the store/disk. Best-effort:
// a failure logs and is dropped — checkpointing is recovery, never
// load-bearing. A no-op unless WithCheckpointer + WithWriter are set.
func (s *Session) captureCheckpoint(seq int) {
	if s.ckptStore == nil || s.ckptMergedDir == "" || s.writer == nil || seq <= 0 {
		return
	}
	go func() {
		s.ckptMu.Lock()
		defer s.ckptMu.Unlock()
		err := session.CaptureCheckpoint(s.ckptStore, s.writer, seq, s.ckptRetain,
			func(ctx context.Context, dst string) error {
				return workspace.SnapshotTree(ctx, s.ckptMergedDir, dst)
			})
		if err != nil {
			slog.Warn("host: checkpoint capture failed", "seq", seq, "err", err)
		}
	}()
}

// loop is the sole reader of the Channel. It never blocks on agent
// work: inference and permission service run in their own goroutines.
func (s *Session) loop(ctx context.Context, ready chan<- error) {
	// loop is the lifecycle owner: when it returns (worker done/error, or
	// the Channel dropped) no more sends can matter, so it stops the writer
	// goroutine. In-flight producer goroutines (serveInference, etc.) then
	// get ErrWriterClosed from send() instead of writing a dead channel.
	defer s.chWriter.Close()
	readyOnce := sync.Once{}
	signalReady := func(err error) { readyOnce.Do(func() { ready <- err }) }

	// lastSeqByAgent remembers the DB seq of the most recently applied
	// message per agent so a following MsgPersistMessageUsage attaches to
	// the right row. The worker ships message then usage in order and this
	// loop is the sole applier, so the map is always current. Keyed per
	// agent (not a single cursor) so interleaved sub-agent appends — if
	// they ever happen — still attribute correctly.
	lastSeqByAgent := map[string]int{}

	for {
		env, err := s.ch.Recv()
		if err != nil {
			// Channel dropped without a terminal message. Before ready
			// this fails the handshake (Start surfaces it). After ready,
			// it is an abnormal exit (container OOM-killed, VM died,
			// worker panic/SIGKILL) UNLESS we asked for the teardown
			// ourselves (Close/CloseInput) or our ctx ended — those EOFs
			// are the expected wind-down and stay a clean nil.
			signalReady(fmt.Errorf("host: worker channel closed before ready: %w", err))
			if s.shutdownRequested.Load() || ctx.Err() != nil {
				s.finish(nil)
				return
			}
			werr := fmt.Errorf("worker channel closed unexpectedly: %w", err)
			// The backend may have captured WHY the box died (podman/lima
			// keep a stderr ring buffer; the same hook Start uses for a
			// failed handshake). Enrich the error when there is anything.
			if d, ok := s.worker.(interface{ StartupDiagnostic() string }); ok {
				if msg := d.StartupDiagnostic(); msg != "" {
					werr = fmt.Errorf("%w\n\n%s", werr, msg)
				}
			}
			s.finish(werr)
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

		case backend.MsgPersistMessage:
			// An isolated worker can't write the host DB; apply its
			// append here. Best-effort: a persist failure must not kill
			// the session loop (the turn already happened) — but it must
			// not be SILENT either, or a dead DB looks like a healthy run.
			if s.writer != nil {
				var pm wire.PersistMessage
				if err := json.Unmarshal(env.Body, &pm); err != nil {
					slog.Warn("host: persist message: decode failed", "err", err)
					break
				}
				// Reasoning is `json:"-"` on llm.Message so it doesn't
				// survive pm.Msg's unmarshal; re-attach from the explicit
				// wire field before persisting (replay-only chain-of-thought).
				pm.Msg.Reasoning = pm.Reasoning
				seq, err := s.writer.AppendMessage(pm.Msg, pm.AgentID)
				if err != nil {
					slog.Warn("host: persist message failed", "agent", pm.AgentID, "err", err)
					// Invalidate the cursor: the following usage record
					// belongs to THIS failed append, so attributing it to
					// the previous row would corrupt accounting. Dropping
					// the stale entry makes the usage handler skip instead.
					delete(lastSeqByAgent, pm.AgentID)
					break
				}
				lastSeqByAgent[pm.AgentID] = seq
			}

		case backend.MsgPersistMessageUsage:
			// Worker reports the usage immediately after its
			// MsgPersistMessage; attribute it to that agent's
			// last-applied message seq.
			if s.writer != nil {
				var pu wire.PersistMessageUsage
				if err := json.Unmarshal(env.Body, &pu); err != nil {
					slog.Warn("host: persist usage: decode failed", "err", err)
					break
				}
				seq, ok := lastSeqByAgent[pu.AgentID]
				if !ok {
					// The matching message append failed (or never arrived);
					// skip rather than misattribute to the wrong row.
					slog.Warn("host: persist usage skipped: no applied message to attach to", "agent", pu.AgentID)
					break
				}
				if err := s.writer.AppendMessageUsage(seq, pu.Usage, pu.AgentID); err != nil {
					slog.Warn("host: persist usage failed", "agent", pu.AgentID, "seq", seq, "err", err)
				}
			}

		case backend.MsgPersistToolCall:
			if s.writer != nil {
				var pt wire.PersistToolCall
				if err := json.Unmarshal(env.Body, &pt); err != nil {
					slog.Warn("host: persist tool call: decode failed", "err", err)
					break
				}
				if err := s.writer.AppendToolCall(pt.CallID, pt.Name, pt.Args, pt.LLMOutput, pt.FullOutput, pt.Status); err != nil {
					slog.Warn("host: persist tool call failed", "call", pt.CallID, "tool", pt.Name, "err", err)
				}
			}

		case backend.MsgCheckpoint:
			// An isolated worker just persisted a genuine user turn and
			// asked us to snapshot the overlay it's working in. Use OUR
			// last-applied top-level seq (the worker's remoteWriter seq is
			// relative and would diverge on resume). The user message was
			// applied immediately before this envelope (ordered seam), so
			// lastSeqByAgent[""] is exactly that turn's host DB seq.
			s.captureCheckpoint(lastSeqByAgent[""])

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
			// Register the per-corr canceller synchronously, BEFORE
			// dispatching the goroutine, so a MsgInferenceCancel that
			// arrives in the next demux iteration can't race past an
			// unregistered corr. Without this, the goroutine might not
			// have scheduled before the cancel envelope is processed,
			// the lookup would miss, and the upstream HTTP would run
			// to completion despite the cancel.
			callCtx, cancel := context.WithCancel(ctx)
			s.infMu.Lock()
			s.infCancel[env.Corr] = cancel
			s.infMu.Unlock()
			go s.serveInference(callCtx, cancel, env.Corr, env.Body)

		case backend.MsgInferenceCancel:
			s.infMu.Lock()
			cancel, ok := s.infCancel[env.Corr]
			s.infMu.Unlock()
			if ok {
				cancel()
			}

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

// AddAllow appends a persistent allow pattern to the worker's REAL
// enforcing checker so subsequent calls in this session don't re-prompt.
// Mirrors SetYolo: the TUI persists the rule to project config separately
// (for future sessions) and mirrors it onto the host display checker.
// A bad pattern is reported via the control response Error.
func (s *Session) AddAllow(ctx context.Context, pattern string) error {
	_, err := s.control(ctx, wire.CtrlAddAllow, wire.ControlName{Name: pattern})
	return err
}

// AddTurnAllow appends a turn-scoped allow pattern to the worker's REAL
// enforcing checker. The grant clears at the next user message (the
// worker agent loop calls ResetTurnAllows); it is never persisted.
func (s *Session) AddTurnAllow(ctx context.Context, pattern string) error {
	_, err := s.control(ctx, wire.CtrlAddTurnAllow, wire.ControlName{Name: pattern})
	return err
}

// RunWorkflow executes a declarative workflow inside the worker — the
// engine, its role agents and their tool calls all run behind the same
// Backend seam as the interactive agent; only inference, permission
// prompts and persistence round-trip to the host, exactly like an
// interactive turn. definition is the RAW workflow markdown (resolved
// and pre-validated host-side; the worker re-parses it, so no shared
// filesystem is assumed). Blocks until the workflow completes — pass a
// long-lived ctx, not a control-RPC timeout. Role progress streams over
// the bus (EventAgentStart/End) while this waits.
func (s *Session) RunWorkflow(ctx context.Context, name string, definition []byte, args string) (wire.WorkflowResult, error) {
	raw, err := s.control(ctx, wire.CtrlRunWorkflow, wire.WorkflowRun{
		Name: name, Definition: definition, Args: args,
	})
	if err != nil {
		return wire.WorkflowResult{}, err
	}
	var res wire.WorkflowResult
	err = json.Unmarshal(raw, &res)
	return res, err
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
//
// callCtx + cancel are registered SYNCHRONOUSLY by the demux loop
// before this goroutine is launched (see MsgInferenceRequest), so a
// MsgInferenceCancel arriving immediately after the request envelope
// always finds the corr in s.infCancel. This goroutine owns cleanup:
// it calls cancel() and deletes the map entry on return.
func (s *Session) serveInference(callCtx context.Context, cancel context.CancelFunc, corr string, body json.RawMessage) {
	defer cancel()
	defer func() {
		s.infMu.Lock()
		delete(s.infCancel, corr)
		s.infMu.Unlock()
	}()

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

	release, err := p.Pool.Acquire(callCtx)
	if err != nil {
		s.sendInferenceError(corr, fmt.Sprintf("pool: %v", err))
		return
	}
	defer release()

	evCh, err := p.Client.Chat(callCtx, ir.Request)
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
