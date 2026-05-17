// SPDX-License-Identifier: AGPL-3.0-or-later

// Package backend defines the seam between the enso host process (TUI,
// supervisor, model endpoint + credentials) and the agent core (model
// loop + tools), which always runs in a separate Worker child process.
//
// There is exactly one execution path. The agent core always runs as a
// Worker behind a Backend. "Sandbox off" is not a separate code path —
// it is a degenerate Backend (LocalBackend: a host child process, no
// container). This makes the historical split-brain bug class
// (bash in a container at /work, file tools on the host at the real
// cwd) structurally impossible: there is no second path for it to live
// in.
//
// This package is intentionally stdlib-only and sits at the bottom of
// the import graph so both the host glue and the `enso __worker`
// entrypoint can depend on it without import cycles. It defines the
// contract and the wire envelope; the host-side and worker-side
// adapters that translate envelopes to/from the agent's concrete types
// (chan string, *bus.Bus, llm.ChatClient, permissions.Decision) live in
// internal/backend/{host,worker}, not here.
package backend

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
)

// Backend provisions an isolated box, launches the agent Worker into it
// with the given TaskSpec, and returns a handle. It is deliberately NOT
// a rich per-tool Exec/Mount surface — those are in-Worker concerns. A
// Backend's whole job is: provision -> launch worker -> hand back a
// control Channel -> tear it all down.
//
// Implementations:
//   - LocalBackend  (sandbox off, the default): Worker is a host child
//     process, no container, no overlay, no network seal, full host env
//     — exactly today's logical semantics. Channel transport is
//     in-process pipes.
//   - PodmanBackend (sandbox on, opt-in): Worker is PID 1 of a
//     `podman run --rm` container; overlay workspace; network-sealed;
//     Channel transport over the daemon protocol socket.
//
// Backend abstracts where / how isolated the Worker runs. It does NOT
// abstract what the agent may do logically: file confinement
// (restrictedRoots) stays a tool-level concern that behaves identically
// regardless of Backend.
type Backend interface {
	// Start provisions the box and launches the Worker. The returned
	// Worker is already running; its Channel is live. spec is the only
	// thing that crosses into the box at launch — it must be fully
	// serializable and must not carry host secrets (see TaskSpec).
	Start(ctx context.Context, spec TaskSpec) (Worker, error)

	// Name is a stable identifier for diagnostics and config
	// ("local", "podman", "lima", ...).
	Name() string
}

// Worker is a launched agent core. The host drives it entirely through
// Channel; Wait observes its lifetime; Teardown reclaims it.
type Worker interface {
	// Channel is the single bidirectional control seam. Everything the
	// network-sealed Worker cannot do itself (inference, human-in-the-
	// loop, brokered capabilities) flows over this; so does agent input
	// and the agent's event output. Stable for the Worker's lifetime.
	Channel() Channel

	// Wait blocks until the Worker process exits or ctx is cancelled.
	// It returns the worker's exit status as an error (nil = clean
	// exit). It does not reclaim resources — call Teardown for that.
	Wait(ctx context.Context) error

	// Teardown reclaims everything this Worker owns: the child process
	// and, for container backends, the container, its overlay upper
	// layer, and any anonymous volumes. Teardown owning the overlay +
	// volumes (not just the container) is part of the contract from the
	// start so it is not bolted on later. Idempotent; safe after Wait.
	Teardown(ctx context.Context) error
}

// TaskSpec is the complete, serializable description of one agent task.
// It is the only thing that crosses into the box at launch.
//
// Invariant — credential scrubbing by construction: TaskSpec carries NO
// host secrets. No endpoints, no API keys, no token-bearing env. The
// Worker never makes a model call itself; inference is host-proxied
// over the Channel (MsgInferenceRequest). Providers below is the
// non-secret catalog (names/models only) the agent needs so provider
// switching and the spawn_agent `model` arg keep working. Anything the
// Worker legitimately needs that would otherwise be a secret is
// requested at runtime as a brokered capability.
type TaskSpec struct {
	// TaskID is the per-task identity. Container backends name the box
	// `enso-<base>-<TaskID>` so concurrent tasks on one project cannot
	// collide (and to anticipate the parallel-task case).
	TaskID string `json:"task_id"`

	// Cwd is the host project directory. For container backends this is
	// the only thing mounted in; there is no host $HOME exposure.
	Cwd string `json:"cwd"`

	// SessionID ties the run to a session store entry (resume, audit).
	SessionID string `json:"session_id,omitempty"`

	// Prompt is the initial user message. Empty is valid (an
	// interactive worker that waits for the first MsgInput).
	Prompt string `json:"prompt,omitempty"`

	// Interactive selects input-stream lifetime, mirroring today's
	// three host shapes: false = one prompt then the input stream
	// closes and the worker exits when quiescent (`enso run`); true =
	// the input stream stays open for follow-up messages (TUI, daemon).
	Interactive bool `json:"interactive,omitempty"`

	// ResumeHistory is an opaque, pre-serialized []llm.Message for
	// resumption. Opaque on purpose: this package stays stdlib-only and
	// at the bottom of the import graph. The worker-side adapter owns
	// the typed (de)serialization. Nil = fresh system prompt.
	ResumeHistory json.RawMessage `json:"resume_history,omitempty"`

	// ResolvedConfig is the host's already-loaded *config.Config,
	// serialized. The worker uses it verbatim rather than re-running
	// config.Load, so host and worker cannot drift on layered-config
	// resolution. Opaque here for the same import-graph reason.
	ResolvedConfig json.RawMessage `json:"resolved_config,omitempty"`

	// Providers is the NON-SECRET provider catalog (see the credential-
	// scrubbing invariant above). Endpoint/APIKey are deliberately
	// absent from ProviderInfo.
	Providers       []ProviderInfo `json:"providers"`
	DefaultProvider string         `json:"default_provider,omitempty"`

	MaxTurns int `json:"max_turns,omitempty"`

	// Logical agent-behavior knobs, forwarded verbatim into the
	// worker's agent.Config. These are not isolation concerns; they
	// behave identically regardless of Backend.
	RestrictedRoots        []string `json:"restricted_roots,omitempty"`
	AdditionalDirectories  []string `json:"additional_directories,omitempty"`
	DisableFileConfinement bool     `json:"disable_file_confinement,omitempty"`
	GitAttribution         string   `json:"git_attribution,omitempty"`
	GitAttributionName     string   `json:"git_attribution_name,omitempty"`
	ExtraSystemPrompt      string   `json:"extra_system_prompt,omitempty"`
	WebFetchAllowHosts     []string `json:"web_fetch_allow_hosts,omitempty"`

	// CLI-flag-driven choices the worker recipe needs that are not
	// derivable from ResolvedConfig. Everything else (hooks, search,
	// mcp, lsp, git attribution, prune, additional dirs, confinement)
	// the worker derives from ResolvedConfig itself.
	Yolo            bool   `json:"yolo,omitempty"`              // --yolo: bypass permission prompts
	AgentProfile    string `json:"agent_profile,omitempty"`     // --agent: declarative profile name
	Ephemeral       bool   `json:"ephemeral,omitempty"`         // --ephemeral: do not persist the session
	ResumeSessionID string `json:"resume_session_id,omitempty"` // --session/--resume/--continue target

	// Isolation describes the box. The zero value means "no isolation"
	// (LocalBackend). Container/VM backends populate it. Defined so
	// the contract is stable; fields are honoured by the container
	// backend.
	Isolation IsolationSpec `json:"isolation,omitzero"`
}

// ProviderInfo is the non-secret view of a configured LLM endpoint.
// Endpoint and APIKey are intentionally not present: the Worker never
// dials a model directly.
type ProviderInfo struct {
	Name  string `json:"name"`
	Model string `json:"model"`
	Pool  string `json:"pool,omitempty"`
}

// IsolationSpec is the box description. Zero value = no isolation
// (LocalBackend: host child process). Honoured by container/VM
// backends; defined so TaskSpec does not need re-versioning.
type IsolationSpec struct {
	// NetworkSealed, when true (container backends), means the Worker
	// has no route out. All egress is a brokered capability over the
	// Channel; inference is host-proxied. LocalBackend leaves this
	// false (full host network, today's behavior).
	NetworkSealed bool `json:"network_sealed,omitempty"`

	// Image is the container image (container backends only).
	Image string `json:"image,omitempty"`

	// ExtraMounts are additional host:container bind mounts beyond the
	// project dir. Container backends only.
	ExtraMounts []string `json:"extra_mounts,omitempty"`

	// Runtime selects an OCI runtime hardening option (e.g. "runsc"
	// for gVisor on the podman backend). Empty = backend default.
	Runtime string `json:"runtime,omitempty"`
}

// ---------------------------------------------------------------------
// Channel: the bidirectional control seam.
// ---------------------------------------------------------------------

// Channel is a typed, bidirectional message conn between host and
// Worker. It is transport-agnostic: LocalBackend backs it with
// in-process OS pipes, PodmanBackend with the daemon protocol socket.
// The framing (NewStreamChannel) is the proven length-prefixed-JSON
// scheme the daemon already uses, generalized to any io stream so it
// works over a pipe.
//
// Send and Recv must each be safe for use from a single goroutine;
// concurrent Send from multiple goroutines must be serialized by the
// caller (the host/worker adapters own one writer goroutine, mirroring
// the daemon).
type Channel interface {
	Send(Envelope) error
	Recv() (Envelope, error)
	Close() error
}

// MsgKind discriminates an Envelope. The taxonomy covers every leg of
// the seam the coupling analysis identified. Tiers are scoped from the
// start (tier-3 capability included as a defined-but-not-yet-served
// kind) specifically so the wire format does not need re-versioning
// when the credential broker is served.
type MsgKind string

const (
	// Handshake. The host sends exactly one MsgTaskSpec as the first
	// envelope (TaskSpec is "the only thing that crosses into the box
	// at launch"); the worker replies MsgWorkerReady once the agent
	// core is constructed. Uniform across LocalBackend (pipe) and
	// PodmanBackend (socket).
	MsgTaskSpec MsgKind = "task_spec" // host -> worker: the TaskSpec (first envelope)

	// Lifecycle.
	MsgWorkerReady MsgKind = "worker_ready" // worker -> host: agent core up, Channel live
	MsgWorkerDone  MsgKind = "worker_done"  // worker -> host: run finished cleanly
	MsgWorkerError MsgKind = "worker_error" // worker -> host: fatal worker error (Body: ErrorBody)
	MsgShutdown    MsgKind = "shutdown"     // host -> worker: close input, wind down

	// Input leg (replaces the chan string passed to Agent.Run).
	MsgInput  MsgKind = "input"  // host -> worker: a user message (Body: InputBody)
	MsgCancel MsgKind = "cancel" // host -> worker: cancel the in-flight turn

	// Output leg (replaces direct *bus.Bus subscription). One Envelope
	// per bus event; payload is the daemon wire shape (Seq/Type/Payload)
	// so existing renderers/daemon fan-out are reused unchanged.
	MsgEvent MsgKind = "event" // worker -> host: a bus event (Body: EventBody)

	// Telemetry leg (replaces the TUI status line / overlay reading
	// token counts + active provider/model directly off *agent.Agent).
	// Fire-and-forget, coalesced: the worker emits only when the
	// snapshot changes. Body is wire.Telemetry.
	MsgTelemetry MsgKind = "telemetry" // worker -> host: agent state snapshot

	// Inference leg (host-proxied; the Worker never dials a model).
	// Request/response correlated by Envelope.Corr.
	MsgInferenceRequest MsgKind = "inference_request" // worker -> host: a serialized llm.ChatRequest
	MsgInferenceEvent   MsgKind = "inference_event"   // host -> worker: one streamed llm.Event
	MsgInferenceDone    MsgKind = "inference_done"    // host -> worker: stream complete
	MsgInferenceError   MsgKind = "inference_error"   // host -> worker: stream failed (Body: ErrorBody)

	// Human-in-the-loop leg. Mirrors daemon's PermissionRequestPayload
	// / PermissionResponseReq, correlated by Envelope.Corr.
	MsgPermissionRequest  MsgKind = "permission_request"  // worker -> host
	MsgPermissionDecision MsgKind = "permission_decision" // host -> worker

	// Control-RPC leg. A single generic correlated request/response
	// pair carrying a method name + JSON args/result (wire.Control*).
	// The TUI drives the worker-side agent synchronously through this
	// for the slash-command / overlay surface (set provider, compact,
	// prune, prefix breakdown, provider context, next-turn tools). One
	// pair with an internal method namespace keeps the taxonomy stable
	// — new control verbs never re-version the wire.
	MsgControlRequest  MsgKind = "control_request"  // host -> worker (Body: wire.ControlRequest)
	MsgControlResponse MsgKind = "control_response" // worker -> host (Body: wire.ControlResponse)

	// Capability/credential broker leg. Defined now so the
	// taxonomy is stable; not served until the broker lands.
	MsgCapabilityRequest MsgKind = "capability_request" // worker -> host
	MsgCapabilityGrant   MsgKind = "capability_grant"   // host -> worker
)

// Envelope is the single wire unit. Body holds the kind-specific
// payload as raw JSON so each side decodes lazily — the same lazy
// envelope discipline the daemon protocol uses. Corr correlates a
// request with its response/stream (inference, permission, capability);
// empty for fire-and-forget kinds.
type Envelope struct {
	Kind MsgKind         `json:"kind"`
	Corr string          `json:"corr,omitempty"`
	Body json.RawMessage `json:"body,omitempty"`
}

// InputBody is the payload for MsgInput.
type InputBody struct {
	Text string `json:"text"`
}

// EventBody is the payload for MsgEvent: the daemon wire form of a bus
// event. Identical shape to daemon.Event so host fan-out/renderers need
// no translation.
type EventBody struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// ErrorBody is the payload for MsgWorkerError / MsgInferenceError.
type ErrorBody struct {
	Message string `json:"message"`
}

// NewBody marshals v and returns it as a json.RawMessage for Envelope.Body.
func NewBody(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("backend: marshal body: %w", err)
	}
	return b, nil
}

// ---------------------------------------------------------------------
// Stream framing: length-prefixed JSON over any io stream.
// ---------------------------------------------------------------------

// maxFrame caps a single envelope at 8 MiB, matching the daemon
// protocol's bound.
const maxFrame = 8 * 1024 * 1024

// streamChannel frames Envelopes as a 4-byte big-endian length prefix
// followed by the JSON body — the daemon's proven scheme, generalized
// from a unix socket to any io.ReadWriteCloser so it works over a pipe
// (LocalBackend) as well as a socket (PodmanBackend).
type streamChannel struct {
	r io.Reader
	w io.Writer
	c io.Closer
}

// NewStreamChannel builds a Channel over a duplex stream. For
// LocalBackend the read and write halves are two OS pipes; rwc bundles
// them with a Closer that closes both.
func NewStreamChannel(rwc io.ReadWriteCloser) Channel {
	return &streamChannel{r: rwc, w: rwc, c: rwc}
}

// NewStreamChannelRW builds a Channel from separate read and write
// halves plus a closer — the shape LocalBackend has (child stdout pipe
// for reads, child stdin pipe for writes).
func NewStreamChannelRW(r io.Reader, w io.Writer, c io.Closer) Channel {
	return &streamChannel{r: r, w: w, c: c}
}

func (s *streamChannel) Send(env Envelope) error {
	buf, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("backend: marshal envelope: %w", err)
	}
	if len(buf) > maxFrame {
		return fmt.Errorf("backend: envelope too large: %d bytes", len(buf))
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(buf)))
	if _, err := s.w.Write(prefix[:]); err != nil {
		return fmt.Errorf("backend: write prefix: %w", err)
	}
	if _, err := s.w.Write(buf); err != nil {
		return fmt.Errorf("backend: write body: %w", err)
	}
	return nil
}

func (s *streamChannel) Recv() (Envelope, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(s.r, prefix[:]); err != nil {
		return Envelope{}, err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n == 0 || n > maxFrame {
		return Envelope{}, fmt.Errorf("backend: framing: bad length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.r, buf); err != nil {
		return Envelope{}, fmt.Errorf("backend: read body: %w", err)
	}
	var env Envelope
	if err := json.Unmarshal(buf, &env); err != nil {
		return Envelope{}, fmt.Errorf("backend: unmarshal envelope: %w", err)
	}
	return env, nil
}

func (s *streamChannel) Close() error {
	if s.c != nil {
		return s.c.Close()
	}
	return nil
}
