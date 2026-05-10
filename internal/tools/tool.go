// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

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

	// Sandbox, when non-nil, routes the bash tool through a container
	// instead of `os/exec` on the host. See internal/sandbox; *Manager
	// satisfies SandboxRunner. Other tools are unaffected.
	Sandbox SandboxRunner

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

	// WebFetchAllowHosts is consulted by the web_fetch tool to permit
	// specific hosts past the SSRF guard's loopback/private-IP block
	// (e.g. a local llama.cpp server). Each entry is "host" or
	// "host:port" — see config.WebFetchConfig.
	WebFetchAllowHosts []string

	// OutputCaps controls per-tool truncation thresholds applied via
	// HeadTail. Zero values fall through to DefaultOutputCap, which
	// itself falls back to 2000 for backward compatibility.
	OutputCaps DefaultOutputCaps

	// RecentUserHint is the most recent user message text — used by
	// RelevantTruncate (B2) as a relevance signal when an output
	// exceeds its cap. Empty disables relevance truncation.
	RecentUserHint string
}

// DefaultOutputCaps lets the host pin per-tool LLMOutput line caps
// without each tool growing its own knob. Read by HeadTail callers
// inside the tools package; the agent.Config plumbs values through.
type DefaultOutputCaps struct {
	Default int            // global default; 0 → 2000
	PerTool map[string]int // tool name → override
}

// CapFor returns the cap for `toolName`, falling back to Default and
// then 2000.
func (c DefaultOutputCaps) CapFor(toolName string) int {
	if c.PerTool != nil {
		if v, ok := c.PerTool[toolName]; ok && v > 0 {
			return v
		}
	}
	if c.Default > 0 {
		return c.Default
	}
	return 2000
}

// FileEditHook is the slice of internal/hooks.Hooks the edit/write
// tools call into post-success. Defined here so this package doesn't
// import internal/hooks.
type FileEditHook interface {
	OnFileEdit(cwd, path, tool string)
}

// SessionWriter is what tools needs from session.Writer to record
// messages and tool calls. Defined here so the tools package doesn't
// import session. Implemented by *session.Writer.
type SessionWriter interface {
	AppendMessage(msg llm.Message, agentID string) error
	AppendToolCall(callID, name string, args map[string]interface{}, llmOutput, fullOutput, status string) error
	SessionID() string
}

// SandboxRunner is the slice of `internal/sandbox.Manager` that the
// bash tool needs. Defined here so the tools package doesn't pull in
// sandbox. Nil on the AgentContext means "run bash on the host".
//
// Image and WorkdirMount are read by the agent when building the
// system-prompt environment note so the model knows it is inside a
// container and what its in-container cwd is.
type SandboxRunner interface {
	Exec(ctx context.Context, w io.Writer, cmd string) error
	ContainerName() string
	Runtime() string
	Image() string
	WorkdirMount() string
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
