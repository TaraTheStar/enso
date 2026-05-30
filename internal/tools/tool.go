// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/permissions"
)

// Tool is the interface for all built-in and MCP-adapted tools.
type Tool interface {
	Name() string
	Description() string
	Parameters() map[string]interface{}
	Run(ctx context.Context, args map[string]interface{}, ac *AgentContext) (Result, error)
}

// Result separates the text fed back to the LLM from the full output stored in the session.
type Result struct {
	LLMOutput     string // truncated text sent back to the model
	FullOutput    string // complete output stored in the session
	DisplayOutput string // optional terse line(s) for scrollback; falls back to LLMOutput when empty
	Display       any    // rich display data (e.g., diff for permission modal)
	Meta          ResultMeta

	// Parts carries non-text content the tool wants the model to see —
	// today that's just images (read tool on a PNG, etc.). When
	// populated, the agent loop wraps the tool-result Message with
	// these Parts so the adapter can translate them to the vendor's
	// multimodal shape. LLMOutput is still set for adapters that don't
	// speak images yet and for session persistence (the DB schema is
	// still TEXT-only); the model "sees" the image via Parts.
	Parts []llm.MessagePart
}

// ResultMeta carries side-channel metadata used by the agent's
// context-pruning machinery. Tools opt into pruning behaviours by
// populating these fields; zero values are safe (no pruning effect).
type ResultMeta struct {
	// PathsRead is the set of absolute file paths whose contents this
	// tool surfaced to the model. The pruner uses this to invalidate
	// stale read results after a write/edit touches the same path
	// (A4), and to decide whether the message references a "pinned"
	// path that should survive stubbing/compaction (C1).
	PathsRead []string

	// PathsWritten is the set of absolute file paths this tool
	// modified. Drives A4 invalidation: any prior read of a path in
	// this set is stubbed when the write/edit message is appended.
	PathsWritten []string

	// CacheKey is a normalized identifier used for same-call dedup
	// (A3). When two tool messages share a CacheKey, the older is
	// stubbed regardless of the per-tool retention threshold.
	// Examples: "read:/abs/path:1-200", "bash:git status".
	CacheKey string
}

// AgentContext carries request-scoped data for tool execution.
type AgentContext struct {
	Cwd         string
	SessionID   string
	Logger      *slog.Logger
	ReadSet     map[string]bool // files read in this session (for write guard)
	Bus         *bus.Bus
	Permissions *permissions.Checker
	MaxTurns    int
	TurnCount   int

	// Capabilities is the tier-3 broker handle. Non-nil only behind the
	// Backend seam (worker); nil on the in-process / LocalBackend path,
	// where tools behave as today (no sealing, no broker). Inherited by
	// spawned sub-agents so a sealed child can still request grants.
	Capabilities CapabilityRequester

	// IsolationNote is the honest one-line box description, inherited by
	// spawned sub-agents so their # Environment section matches the
	// parent's (a child shares the same box).
	IsolationNote string

	// CurrentToolID is set by the agent loop just before each Tool.Run so
	// long-running tools (e.g. bash) can publish EventToolCallProgress
	// events tagged with the originating call's id. Cleared after Run.
	CurrentToolID string

	// AgentID identifies this agent. Empty for the top-level agent. Set
	// by spawn_agent and workflow.runRole when constructing a child so
	// EventAgentStart payloads can carry both `id` and `parent_id` for
	// the agents-pane tree.
	AgentID string

	// AgentRole is the human-readable label for this agent — workflow
	// role name ("reviewer") or spawn_agent's `role` arg. Empty for the
	// top-level agent. Surfaced in permission prompts so the user can
	// tell which subagent is asking.
	AgentRole string

	// Subagent fields. Populated by the parent Agent so the spawn_agent tool
	// can construct a child agent that shares the parent's provider, bus, and
	// permissions. Depth/GlobalAgents/MaxDepth/MaxAgents enforce recursion
	// limits across the entire agent tree.
	//
	// Provider is the parent's *current* active provider — it tracks
	// /model swaps via Agent.SetProvider so a child spawned after the
	// switch inherits the new provider by default. spawn_agent's
	// per-call `model` arg picks a different one out of Providers.
	Provider     *llm.Provider
	Providers    map[string]*llm.Provider // full configured set; spawn_agent's `model` arg looks up here
	Registry     *Registry
	Depth        int
	MaxDepth     int
	GlobalAgents *atomic.Int64
	MaxAgents    int

	// Transcripts, when non-nil, captures completed sub-agents' message
	// histories so the agents-pane click-to-expand overlay can render
	// them. Spawned tools and workflow roles call Store post-RunOneShot.
	Transcripts *Transcripts

	// Writer, when non-nil, lets sub-agents persist their own message
	// rows attributed to their AgentID. The top-level agent's writer is
	// passed through here so spawned children can record transcripts to
	// the same session. Typed as an interface to keep tools out of the
	// session package's import graph; *session.Writer satisfies it.
	Writer SessionWriter

	// RestrictedRoots, when non-empty, gates file-touching tools
	// (read/write/edit/grep/glob/lsp_*) so they refuse paths that
	// don't resolve under one of these roots. Default-populated as
	// `[cwd, ...AdditionalDirectories]` by the host (tui/run/daemon)
	// regardless of bash sandbox setting; users opt out via
	// permissions.disable_file_confinement.
	RestrictedRoots []string

	// FileEditHook, when non-nil, fires after the edit/write tools
	// succeed. Used for auto-format, post-edit linting, etc.
	// *hooks.Hooks satisfies it; nil disables the hook.
	FileEditHook FileEditHook

	// LSPNotifier, when non-nil, is called by write/edit after a
	// successful save to surface language-server diagnostics for the
	// just-touched file. Sibling to FileEditHook (which fires the
	// user's shell-level on_file_edit); LSPNotifier handles the
	// internal-only LSP push so the model learns about compile errors
	// without an extra tool call.
	LSPNotifier LSPNotifier

	// WebFetchAllowHosts is consulted by the web_fetch tool to permit
	// specific hosts past the SSRF guard's loopback/private-IP block
	// (e.g. a local llama.cpp server). Each entry is "host" or
	// "host:port" — see config.WebFetchConfig.
	WebFetchAllowHosts []string

	// OutputCaps controls per-tool truncation thresholds applied via
	// HeadTail. Zero values fall through to DefaultOutputCap, which
	// itself falls back to 2000 for backward compatibility.
	OutputCaps DefaultOutputCaps

	// ToolTimeouts bounds how long a single bash command may run before
	// it is killed. Zero values fall back to the built-in defaults via the
	// accessor methods, so a nil-ish AgentContext still gets guarded bash.
	ToolTimeouts ToolTimeouts

	// BashJobs is the registry of background commands started via the
	// bash tool's run_in_background mode. Each agent (top-level and every
	// sub-agent) gets its own so bash_output/bash_kill only see this
	// agent's jobs and KillAll on teardown can't touch a sibling's.
	// nil disables background mode (the tools report it unavailable).
	BashJobs *BashJobs

	// RecentUserHint is the most recent user message text — used by
	// RelevantTruncate (B2) as a relevance signal when an output
	// exceeds its cap. Empty disables relevance truncation.
	RecentUserHint string

	// Spill, when non-nil, is called by truncateWithRecovery when a
	// tool's output exceeds its caps. The returned path is embedded in
	// the LLMOutput so the model can recover sections via the `read`
	// tool (or filter via `grep`). Nil disables recovery; the model
	// then just sees the head/tail truncated form and the original
	// FullOutput stays in the session DB but isn't reachable.
	Spill SpillWriter

	// Checkpoint, when non-nil, is the seam the `checkpoint` tool uses
	// to ask the agent loop to run a compaction pass before the next
	// model completion. *agent.Agent satisfies it; spawn paths may pass
	// nil so subagents can't compact the top-level history.
	Checkpoint CheckpointRequester

	// InstructionResolver, when non-nil, is the seam the `read` tool
	// uses for contextual ENSO.md / AGENTS.md injection. Called with
	// the absolute path of a just-read file; the implementation walks
	// up to the cwd collecting any directory-scoped instruction files
	// NOT already in the static system prompt or previously injected
	// this session. Returns formatted reminder text (or "" when
	// nothing new) to append to the LLM-visible result. *agent.Agent
	// satisfies it; tests pass nil.
	InstructionResolver InstructionResolver
}

// InstructionResolver is the seam tools use to ask the agent for any
// directory-scoped instruction content to attach to a tool result.
// Defined here (rather than in internal/instructions) so the tools
// package doesn't have to import instructions just for the type.
type InstructionResolver interface {
	// ResolveOnRead returns reminder text to append to the LLMOutput
	// of a successful `read` of absPath. Implementations track which
	// files have already produced reminders this session so the same
	// instructions are not re-injected on subsequent reads. The
	// returned string, when non-empty, should already include any
	// <system-reminder>...</system-reminder> wrapping the model
	// expects to see.
	ResolveOnRead(absPath string) string
}

// DefaultOutputCaps lets the host pin per-tool truncation thresholds
// without each tool growing its own knob. Read by capTruncate callers
// inside the tools package; the agent.Config plumbs values through.
//
// Three independent dimensions are capped, applied in this order
// inside capTruncate: byte cap → line cap → per-line length cap. Each
// cap is independent — a tool result can be byte-capped without
// hitting the line cap, or vice versa.
type DefaultOutputCaps struct {
	Default int            // global line cap; 0 → 2000
	PerTool map[string]int // tool name → line cap override

	// MaxBytes is the global byte ceiling for one tool result. 0 →
	// DefaultMaxBytes (50 KB). Defends against pathological single-line
	// outputs (a minified-JS dump, a binary glob) that line counting
	// can't catch.
	MaxBytes int
	// PerToolBytes overrides MaxBytes per tool name. Same lookup rules
	// as PerTool. Value 0 means "fall through to MaxBytes".
	PerToolBytes map[string]int

	// MaxLineLength is the global per-line character ceiling. 0 →
	// DefaultMaxLineLength (2000 chars). Long lines get their middle
	// elided so a result staying under the line cap can't sneak a
	// 10 MB minified line past the byte cap on a near-edge input.
	MaxLineLength int
	// PerToolLineLength overrides MaxLineLength per tool name.
	PerToolLineLength map[string]int
}

// DefaultMaxBytes / DefaultMaxLineLength are the fallbacks when the
// config leaves the respective cap unset. Picked to match opencode's
// defaults so two systems pointed at the same model see similar tool
// result sizing.
const (
	DefaultMaxBytes      = 50 * 1024
	DefaultMaxLineLength = 2000
	defaultLineCap       = 2000
)

// CapFor returns the line cap for `toolName`, falling back to Default
// and then defaultLineCap.
func (c DefaultOutputCaps) CapFor(toolName string) int {
	if c.PerTool != nil {
		if v, ok := c.PerTool[toolName]; ok && v > 0 {
			return v
		}
	}
	if c.Default > 0 {
		return c.Default
	}
	return defaultLineCap
}

// BytesFor returns the byte cap for `toolName`, falling back to
// MaxBytes and then DefaultMaxBytes.
func (c DefaultOutputCaps) BytesFor(toolName string) int {
	if c.PerToolBytes != nil {
		if v, ok := c.PerToolBytes[toolName]; ok && v > 0 {
			return v
		}
	}
	if c.MaxBytes > 0 {
		return c.MaxBytes
	}
	return DefaultMaxBytes
}

// LineLengthFor returns the per-line character cap for `toolName`,
// falling back to MaxLineLength and then DefaultMaxLineLength.
func (c DefaultOutputCaps) LineLengthFor(toolName string) int {
	if c.PerToolLineLength != nil {
		if v, ok := c.PerToolLineLength[toolName]; ok && v > 0 {
			return v
		}
	}
	if c.MaxLineLength > 0 {
		return c.MaxLineLength
	}
	return DefaultMaxLineLength
}

// Default tool timeouts, used when AgentContext.ToolTimeouts leaves a field
// zero. Mirror config.DefaultBashCommandTimeout* — duplicated here (rather
// than imported) so the tools package stays free of a config dependency on
// this hot path and tests get guarded bash without wiring.
const (
	defaultBashTimeout    = 120 * time.Second
	defaultBashTimeoutMax = time.Hour
)

// ToolTimeouts bounds bash command execution. The zero value still guards:
// an unspecified call gets the default budget, and an explicit `timeout` is
// honoured verbatim up to the (generous) ceiling.
type ToolTimeouts struct {
	// BashDefault is the wall-clock budget applied to a foreground bash
	// command when the call doesn't supply its own `timeout`. A value < 0
	// means "disabled" (no timeout); 0 means "use the default".
	BashDefault time.Duration
	// BashMax is the hard ceiling on a model-supplied `timeout` — a runaway
	// backstop set generously (1h default) so it never bites a legitimate
	// slow-but-finite job (a big test suite, a long build) but still bounds
	// a hallucinated absurd value. 0 → default ceiling. Set it via
	// [bash] command_timeout_max to widen or tighten the cap.
	BashMax time.Duration
}

// bashMax returns the effective ceiling for a model-supplied timeout.
func (t ToolTimeouts) bashMax() time.Duration {
	if t.BashMax > 0 {
		return t.BashMax
	}
	return defaultBashTimeoutMax
}

// EffectiveBash returns the wall-clock timeout to apply to a foreground
// bash command. requestedSecs is the model's `timeout` arg in seconds (0 =
// unset). An explicit value is honoured verbatim up to the bashMax ceiling;
// when unset the configured default applies. A returned 0 means "no
// timeout".
func (t ToolTimeouts) EffectiveBash(requestedSecs int) time.Duration {
	if requestedSecs > 0 {
		d := time.Duration(requestedSecs) * time.Second
		if max := t.bashMax(); d > max {
			d = max
		}
		return d
	}
	switch {
	case t.BashDefault < 0:
		return 0 // explicitly disabled by config ("0s")
	case t.BashDefault == 0:
		return defaultBashTimeout
	default:
		return t.BashDefault
	}
}

// FileEditHook is the slice of internal/hooks.Hooks the edit/write
// tools call into post-success. Defined here so this package doesn't
// import internal/hooks.
type FileEditHook interface {
	OnFileEdit(cwd, path, tool string)
}

// LSPNotifier is the seam write/edit use to surface live language-
// server diagnostics for a just-edited file. Defined here so the
// tools package doesn't need to import internal/lsp.
//
// NotifyWrite blocks up to the implementation's wait window for the
// server to publish diagnostics for absPath. The returned string,
// when non-empty, is appended verbatim to the tool's LLMOutput so the
// model sees compile errors in the same turn. Empty return = nothing
// worth surfacing (no LSP configured for this language, server crashed,
// no diagnostics produced, etc.); callers must treat the call as
// best-effort and never let it fail the edit itself.
type LSPNotifier interface {
	NotifyWrite(ctx context.Context, absPath string) string
}

// SessionWriter is what tools needs from session.Writer to record
// messages and tool calls. Defined here so the tools package doesn't
// import session. Implemented by *session.Writer.
//
// AppendMessageUsage records provider-reported token counts for the
// most recently appended message in this writer's session. Must be
// called immediately after AppendMessage (before any other Append*
// call) so the writer's internal seq cursor still points at the
// message the usage describes. No-op semantics are acceptable when
// the writer can't apply (e.g. usage arrives without a prior
// AppendMessage); implementations should not return an error in that
// case but may log.
type SessionWriter interface {
	AppendMessage(msg llm.Message, agentID string) error
	AppendMessageUsage(usage llm.MessageUsage, agentID string) error
	AppendToolCall(callID, name string, args map[string]interface{}, llmOutput, fullOutput, status string) error
	SessionID() string
}

// Transcripts is a goroutine-safe map from agent id to that agent's full
// message history at completion. Populated by spawn_agent and workflow
// roles; read by the TUI's agents pane to render an expand-into-chat
// overlay.
type Transcripts struct {
	mu sync.Mutex
	m  map[string][]llm.Message
}

// NewTranscripts constructs an empty registry.
func NewTranscripts() *Transcripts {
	return &Transcripts{m: map[string][]llm.Message{}}
}

// Store records a copy of `history` keyed by `agentID`. Calling with the
// same id overwrites — the latest run wins.
func (t *Transcripts) Store(agentID string, history []llm.Message) {
	if t == nil || agentID == "" {
		return
	}
	cp := make([]llm.Message, len(history))
	copy(cp, history)
	t.mu.Lock()
	t.m[agentID] = cp
	t.mu.Unlock()
}

// Get returns the captured transcript for `agentID`, or nil if none.
// Returned slice is safe to iterate but should not be mutated.
func (t *Transcripts) Get(agentID string) []llm.Message {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.m[agentID]
}

// IDs returns the agent IDs of every stored transcript. Order is
// undefined (Go map iteration). Used by the /transcript slash
// command to enumerate available transcripts.
func (t *Transcripts) IDs() []string {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.m))
	for k := range t.m {
		out = append(out, k)
	}
	return out
}
