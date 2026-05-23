// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/config"
	"github.com/TaraTheStar/enso/internal/hooks"
	"github.com/TaraTheStar/enso/internal/instructions"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/paths"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/tools"
)

const (
	defaultMaxTurns  = 50
	defaultMaxDepth  = 3
	defaultMaxAgents = 16
)

// Agent owns the conversation, providers, tool registry, and event bus.
//
// Multi-provider: an Agent is constructed with a non-empty Providers
// map and a name that selects the active one. The active provider can
// be swapped mid-session via SetProvider; reads must go through the
// Provider() method (mutex-guarded). The agent's tool calls and
// streaming chat all route through whichever provider is current at
// the moment of the call.
type Agent struct {
	Providers map[string]*llm.Provider
	History   []llm.Message
	Bus       *bus.Bus
	Registry  *tools.Registry
	Perms     *permissions.Checker
	AgentCtx  *tools.AgentContext
	Writer    tools.SessionWriter // optional; nil = ephemeral (or a remote, seam-backed writer for isolated backends)
	MaxTurns  int
	Hooks     *hooks.Hooks // optional; on_session_end fires from Run's defer

	mu        sync.Mutex
	curCancel context.CancelFunc

	// currentProvider is the active provider for new turns. Read with
	// Provider(); write with SetProvider(). The same `mu` mutex
	// protects this and curCancel — neither is contended on a hot path.
	currentProvider *llm.Provider

	// nextTurnTools is a one-shot allow-list applied to the registry at
	// the start of the next runUntilQuiescent. Used by skills'
	// `allowed-tools` to restrict the model to a subset for one user
	// turn. Cleared after consumption.
	nextTurnTools []string

	// compactNext is the one-shot flag the `checkpoint` tool flips. The
	// next MaybeCompact bypasses the threshold check and runs a forced
	// compaction with compactReason as the trigger reason. Both reset
	// after consumption.
	compactNext   atomic.Bool
	compactReason atomic.Pointer[string]

	// estTokens caches the best available token-count estimate for
	// History so the UI goroutine can read without racing the agent
	// goroutine's mutations. Updated on every appendMessage and after
	// compaction. Prefers provider-reported usage from lastUsage when
	// present; falls back to the 4-char heuristic otherwise.
	estTokens atomic.Int64

	// cumIn / cumOut accumulate input and output tokens across every
	// chat completion in this session — different from estTokens
	// (which is the *current* context-window usage). Compaction
	// shrinks estTokens but never these two; they reflect spend, not
	// pressure. Populated from provider-reported usage on each
	// EventUsage; backends that don't report usage (some llama.cpp
	// builds) leave these at 0 for the affected turns.
	cumIn  atomic.Int64
	cumOut atomic.Int64

	// lastUsage is the provider-reported usage from the most recently
	// completed assistant turn. nil until the first EventUsage
	// arrives; cleared by compaction since the InputTokens count
	// reflects a History prefix that no longer exists. estimateTokens
	// falls back to llm.Estimate when nil.
	lastUsage atomic.Pointer[llm.MessageUsage]

	// messageUsage sidecars History — keyed by History index (the slot
	// of each assistant message whose usage we recorded). Indices
	// shift on compaction; the map is cleared at that point and
	// repopulated by subsequent turns.
	messageUsage   map[int]llm.MessageUsage
	messageUsageMu sync.Mutex

	// pruneCfg drives the stale-tool-stubbing + dedup + post-edit
	// invalidation machinery in prune.go. Resolved once at New().
	pruneCfg config.ResolvedPruneConfig

	// toolMeta sidecars History — keyed by message index (the History
	// slot of each `role: "tool"` message). Holds the bookkeeping
	// pruning needs that we can't (or shouldn't) round-trip through
	// llm.Message itself: source toolName, dedup CacheKey, written
	// paths, the user-turn count at append time, whether the message
	// is currently a stub, whether the message is pinned (C1).
	//
	// Indices shift on compaction; rebuildToolMeta() rewrites the map
	// after compaction trims History.
	toolMeta map[int]*toolMessageMeta

	// userTurnCounter increments once per user-message append. It is
	// the basis for "how many turns ago was this tool message
	// added." Distinct from AgentContext.TurnCount, which resets per
	// user message and counts inner-loop iterations.
	userTurnCounter int

	// providerCtx is the input to the auto-rendered "## Available
	// models" prompt section, captured at New() so /prompt can show the
	// same layered breakdown the agent was built with. nil when the
	// section is suppressed (opt-out or <2 providers).
	//
	// Static for the session: the system prompt is built once into
	// History[0] and sent verbatim (compaction preserves it), so a
	// mid-session /model swap does NOT rewrite the "running as" line.
	// Live-updating it would need a second prompt-construction path that
	// drifts from New(), or in-band sentinel markers — both
	// disproportionate to one cosmetic self-ID line. The provider *list*
	// (the actual routing value) is always correct; only "running as"
	// goes stale until next session. The clean fix, if ever needed, is
	// architectural: regenerate the system message per turn.
	providerCtx *instructions.ProviderContext

	// compactionProviderName is the [providers.X] key (from
	// `[compaction] provider`) routing summarisation calls to a
	// dedicated endpoint. Empty = use the session's current provider.
	// Resolved lazily by compactionProvider() so a runtime /model swap
	// or a /reload that adds the provider can take effect mid-session.
	compactionProviderName string

	// injectedInstructions tracks which deep ENSO.md / AGENTS.md files
	// have already been surfaced via contextual injection in this
	// session. Keyed by absolute path. Survives compaction (the content
	// is in History already; re-injecting on every read would burn
	// cache budget). Top-level only — sub-agents get their own tracker
	// via spawn_agent's `New`.
	injectedInstructions   map[string]struct{}
	injectedInstructionsMu sync.Mutex
}

// providerContext builds the *instructions.ProviderContext for the
// auto-rendered "## Available models" section. Returns nil when the
// section is suppressed: fewer than two endpoints, or the user opted out
// via [instructions] include_providers=false (the resolved flag is
// mirrored onto every Provider, so sub-agents inherit it for free).
func providerContext(providers map[string]*llm.Provider, active string) *instructions.ProviderContext {
	if len(providers) < 2 {
		return nil
	}
	if p, ok := providers[active]; ok && !p.IncludeProviders {
		return nil
	}
	infos := make([]instructions.ProviderInfo, 0, len(providers))
	for _, p := range providers {
		infos = append(infos, instructions.ProviderInfo{
			Name:          p.Name,
			Model:         p.Model,
			ContextWindow: p.ContextWindow,
			Description:   p.Description,
			Pool:          p.PoolName,
			SwapCost:      p.PoolSwapCost,
		})
	}
	return &instructions.ProviderContext{Active: active, Providers: infos}
}

// ProviderCtx returns the provider context the system prompt was built
// with (nil when the auto-section is suppressed). Used by /prompt to
// reproduce the exact layered breakdown.
func (a *Agent) ProviderCtx() *instructions.ProviderContext { return a.providerCtx }

// pickDefaultProvider validates `requested` against the providers map.
// If `requested` is non-empty and missing, that's an error. If empty,
// the alphabetically-first key wins — deterministic regardless of map
// iteration order.
func pickDefaultProvider(providers map[string]*llm.Provider, requested string) (string, error) {
	if requested != "" {
		if _, ok := providers[requested]; !ok {
			names := sortedNames(providers)
			return "", fmt.Errorf("default_provider %q not in [providers] (configured: %v)", requested, names)
		}
		return requested, nil
	}
	names := sortedNames(providers)
	return names[0], nil
}

func sortedNames(providers map[string]*llm.Provider) []string {
	out := make([]string, 0, len(providers))
	for name := range providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// fileEditHookOf returns h as a tools.FileEditHook, or a nil interface
// if h itself is nil — so the AgentContext sees a clean nil instead of
// a typed-nil that would falsely succeed the != nil guard in the
// edit/write tools.
func fileEditHookOf(h *hooks.Hooks) tools.FileEditHook {
	if h == nil {
		return nil
	}
	return h
}

// SetNextTurnTools sets a one-shot tool restriction for the next user turn.
// Pass nil or an empty slice to clear. Concurrency-safe.
func (a *Agent) SetNextTurnTools(names []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(names) == 0 {
		a.nextTurnTools = nil
		return
	}
	a.nextTurnTools = append([]string{}, names...)
}

// consumeNextTurnTools returns and clears the one-shot tool restriction.
func (a *Agent) consumeNextTurnTools() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.nextTurnTools
	a.nextTurnTools = nil
	return out
}

// RequestCheckpoint is the seam the `checkpoint` tool calls. It queues a
// forced compaction for the next MaybeCompact pass (i.e. before the next
// model completion in the current runUntilQuiescent). Reason flows into
// the EventCompacted payload and the summariser's seed.
func (a *Agent) RequestCheckpoint(reason string) {
	r := reason
	a.compactReason.Store(&r)
	a.compactNext.Store(true)
}

// consumeCheckpointRequest returns (true, reason) once if a checkpoint
// was requested since the last call, and (false, "") otherwise. The
// flag and reason are cleared atomically so a single request only fires
// one compaction.
func (a *Agent) consumeCheckpointRequest() (bool, string) {
	if !a.compactNext.Swap(false) {
		return false, ""
	}
	rp := a.compactReason.Swap(nil)
	if rp == nil {
		return true, ""
	}
	return true, *rp
}

// EstimateTokens returns the cached token-count estimate for the current
// history. Safe to call from any goroutine.
func (a *Agent) EstimateTokens() int { return int(a.estTokens.Load()) }

// CumulativeInputTokens returns the running total of tokens sent to
// the model across the lifetime of this session (not just the current
// context window). Atomic so the sidebar's render goroutine can read
// safely while the agent goroutine increments mid-turn.
func (a *Agent) CumulativeInputTokens() int64 { return a.cumIn.Load() }

// CumulativeOutputTokens returns the running total of tokens the
// model has produced (assistant content + tool-call args) across the
// session. See CumulativeInputTokens for concurrency notes.
func (a *Agent) CumulativeOutputTokens() int64 { return a.cumOut.Load() }

// Provider returns the agent's active provider. Safe to call from any
// goroutine. The pointer itself is stable for the duration of one
// turn — SetProvider swaps it atomically between turns.
func (a *Agent) Provider() *llm.Provider {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.currentProvider
}

// ProviderName returns the name (config key) of the active provider,
// or "" if none. Convenience for status-line / slash commands.
func (a *Agent) ProviderName() string {
	p := a.Provider()
	if p == nil {
		return ""
	}
	return p.Name
}

// SetProvider switches the active provider by name. Returns an error
// if the name isn't in the configured Providers map. The new provider
// takes effect on the next turn — any in-flight turn finishes on the
// old provider (the Provider() call inside the chat goroutine snapped
// the value before SetProvider could run).
//
// Also updates AgentCtx.Provider so spawn_agent and other tools that
// rely on AgentContext for provider context see the new selection on
// their next invocation.
func (a *Agent) SetProvider(name string) error {
	a.mu.Lock()
	p, ok := a.Providers[name]
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown provider %q", name)
	}
	a.mu.Lock()
	a.currentProvider = p
	if a.AgentCtx != nil {
		a.AgentCtx.Provider = p
	}
	a.mu.Unlock()
	return nil
}

// ContextWindow returns the active provider's configured context
// window (0 if unconfigured).
func (a *Agent) ContextWindow() int {
	p := a.Provider()
	if p == nil {
		return 0
	}
	return p.ContextWindow
}

// refreshEstimate recomputes the cached token count from the current history.
// Must be called whenever history changes — appendMessage and compaction.
func (a *Agent) refreshEstimate() {
	a.estTokens.Store(int64(a.estimateTokens()))
}

// estimateTokens returns the best available token count for the current
// History. Prefers provider-reported usage from the most recent
// assistant turn (real numbers); falls back to llm.Estimate when no
// usage has arrived yet. Adds an estimate of any messages appended
// since the last usage event so we don't undercount tool results and
// the next user message — they aren't in lastUsage.InputTokens but
// will be on the next API call.
func (a *Agent) estimateTokens() int {
	if u := a.lastUsage.Load(); u != nil && !u.Empty() {
		base := u.InputTokens + u.CacheReadTokens + u.CacheWriteTokens
		return base + a.tokensAppendedSinceLastUsage()
	}
	return llm.Estimate(a.History)
}

// tokensAppendedSinceLastUsage estimates the size of messages added to
// History after the last assistant turn whose usage we recorded.
// Returns 0 when no usage has been recorded yet or when the recorded
// turn IS the last message.
func (a *Agent) tokensAppendedSinceLastUsage() int {
	a.messageUsageMu.Lock()
	defer a.messageUsageMu.Unlock()
	lastIdx := -1
	for i := range a.messageUsage {
		if i > lastIdx {
			lastIdx = i
		}
	}
	if lastIdx < 0 || lastIdx >= len(a.History)-1 {
		return 0
	}
	return llm.Estimate(a.History[lastIdx+1:])
}

// LastUsage returns a copy of the provider-reported usage from the most
// recent assistant turn, or the zero value if no usage has been
// recorded. Safe to call from any goroutine.
func (a *Agent) LastUsage() llm.MessageUsage {
	if u := a.lastUsage.Load(); u != nil {
		return *u
	}
	return llm.MessageUsage{}
}

// stampUsage records provider-reported usage for the assistant message
// at History index historyIdx. No-op when usage is empty (provider
// didn't supply numbers — keep lastUsage at whatever it was and don't
// pollute messageUsage with zero rows). Updates cumIn/cumOut from the
// real values; persists via the Writer when available.
func (a *Agent) stampUsage(historyIdx int, usage llm.MessageUsage) {
	if usage.Empty() {
		return
	}
	a.messageUsageMu.Lock()
	a.messageUsage[historyIdx] = usage
	a.messageUsageMu.Unlock()
	a.lastUsage.Store(&usage)
	// Cumulative-spend: real numbers. CacheRead/CacheWrite count
	// against the input side; reasoning rides on output.
	a.cumIn.Add(int64(usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens))
	a.cumOut.Add(int64(usage.OutputTokens + usage.ReasoningTokens))
	a.refreshEstimate()
	if a.Writer != nil {
		if err := a.Writer.AppendMessageUsage(usage, a.AgentCtx.AgentID); err != nil {
			a.AgentCtx.Logger.Error("session: append usage", "err", err)
		}
	}
}

// Config bundles construction parameters for New.
type Config struct {
	// Providers is the full set of configured LLM endpoints, keyed by
	// the user-facing label (e.g. "qwen-fast"). Must be non-empty.
	Providers map[string]*llm.Provider
	// DefaultProvider names which entry in Providers is active at
	// construction. Empty = pick the alphabetically-first key. Validated
	// by New: a non-empty value that doesn't match any key is an error.
	DefaultProvider string

	Bus       *bus.Bus
	Registry  *tools.Registry
	Perms     *permissions.Checker
	Writer    tools.SessionWriter
	History   []llm.Message // optional; if non-nil, replaces the default system prompt
	Cwd       string
	SessionID string
	MaxTurns  int

	// MessageUsage rehydrates the agent's per-message usage sidecar on
	// resume. Keys are History indices; values are the provider-
	// reported token counts. Optional — empty means a fresh session or
	// one that predates real-token-accounting; the first new
	// EventUsage will repopulate.
	MessageUsage map[int]llm.MessageUsage
	// LastUsage seeds Agent.lastUsage on resume so the first
	// MaybeCompact check after resume reads real numbers instead of
	// falling back to the heuristic. nil leaves lastUsage unset.
	LastUsage *llm.MessageUsage

	// Subagent fields. The top-level agent leaves these zero; subagents
	// inherit them via spawn_agent so depth limits and a shared global
	// counter survive across the tree.
	Depth        int
	MaxDepth     int
	MaxAgents    int
	GlobalAgents *atomic.Int64

	// AgentID, when non-empty, identifies this agent in the agents pane.
	// Top-level agents leave it empty; spawn_agent and workflow.runRole
	// allocate one.
	AgentID string

	// AgentRole is the human-readable label for this agent (workflow
	// role name, spawn_agent's `role` arg). Empty for the top-level
	// agent. Forwarded to AgentContext + permission prompts so the user
	// sees which subagent is asking.
	AgentRole string

	// Transcripts, if non-nil, is the registry that spawn_agent /
	// workflow.runRole writes child histories into post-completion so the
	// agents-pane overlay can replay them.
	Transcripts *tools.Transcripts

	// GitAttribution selects the trailer style the model should use when
	// it writes git commit messages: "co-authored-by", "assisted-by", or
	// "none"/"" (no instruction added). GitAttributionName is the name to
	// embed in the trailer; defaults to "enso" when blank.
	GitAttribution     string
	GitAttributionName string

	// ExtraSystemPrompt is appended to the base system prompt with a
	// blank-line separator. Declarative agents (`agents.Spec.PromptAppend`)
	// land here.
	ExtraSystemPrompt string

	// AdditionalDirectories are extra workspace dirs the model should be
	// aware of (in addition to Cwd). Mentioned in the system prompt;
	// surfaced in the @-file picker. Tool calls already accept any path,
	// so this is informational unless paired with permission patterns.
	AdditionalDirectories []string

	// RestrictedRoots, when non-empty, is forwarded to AgentContext so
	// file-touching tools refuse paths outside the allowed roots. Wired
	// from `cwd + cfg.Permissions.AdditionalDirectories` by the host
	// (tui/run/daemon) by default; the user opts out via
	// permissions.disable_file_confinement.
	RestrictedRoots []string

	// Hooks fires user-configured shell commands at lifecycle moments.
	// Forwarded into AgentContext for the file-edit event; the agent
	// itself owns the on-session-end fire from its Run() defer.
	// nil disables hooks.
	Hooks *hooks.Hooks

	// WebFetchAllowHosts is forwarded to AgentContext so the web_fetch
	// tool can permit specific local services (e.g. a llama.cpp server)
	// past the loopback/private-IP block. Empty = strict default.
	WebFetchAllowHosts []string

	// PruneCfg controls the context-pruning subsystem (stale tool
	// stubbing, dedup, post-edit invalidation, compaction pinning).
	// Zero value = defaults via config.ContextPruneConfig.Resolve().
	PruneCfg config.ResolvedPruneConfig

	// CompactionProvider names the [providers.X] entry to route
	// summarisation calls through. Empty = use the session's primary
	// provider. When the named provider is missing in Providers, the
	// agent logs a warning and falls back to the session provider.
	CompactionProvider string

	// LSPNotifier, when non-nil, is propagated to AgentContext so
	// the write/edit tools can surface live language-server
	// diagnostics in their tool results. Constructed worker-side
	// (where the lsp.Manager lives); nil disables the path and
	// matches pre-Phase-1 behaviour.
	LSPNotifier tools.LSPNotifier

	// Capabilities is the tier-3 broker handle, forwarded to
	// AgentContext (and inherited by spawned sub-agents). Non-nil only
	// behind the Backend seam; nil elsewhere (tools behave as today).
	Capabilities tools.CapabilityRequester

	// IsolationNote is one honest sentence describing the box this
	// agent runs in (supplied by the Backend seam: "none — direct
	// host…" for LocalBackend, "container … network sealed" for
	// PodmanBackend). Surfaced verbatim in the # Environment prompt
	// section. Empty → environmentNote states the conservative truth.
	IsolationNote string
}

// normalizeWriter collapses a typed-nil SessionWriter (a nil
// *session.Writer — or any nil pointer — assigned by a caller into the
// interface) to a true nil interface. Without this, such a value is
// non-nil as an interface, so the `if Writer != nil` guards in the
// agent loop pass and AppendMessage dereferences nil (CI segfault via
// internal/workflow). One defensive point covers every caller.
func normalizeWriter(w tools.SessionWriter) tools.SessionWriter {
	if w == nil {
		return nil
	}
	if rv := reflect.ValueOf(w); rv.Kind() == reflect.Pointer && rv.IsNil() {
		return nil
	}
	return w
}

// New creates an Agent with a system prompt built from the three-tier loader.
// If cfg.History is non-empty it is used verbatim (e.g. when resuming).
func New(cfg Config) (*Agent, error) {
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("agent: at least one provider required")
	}
	// cfg is by value; normalizing here makes every downstream use
	// (ac.Writer, a.Writer, spawned children) safe in one place.
	cfg.Writer = normalizeWriter(cfg.Writer)
	defaultName, err := pickDefaultProvider(cfg.Providers, cfg.DefaultProvider)
	if err != nil {
		return nil, err
	}

	maxTurns := cfg.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}
	maxDepth := cfg.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	maxAgents := cfg.MaxAgents
	if maxAgents <= 0 {
		maxAgents = defaultMaxAgents
	}
	globalAgents := cfg.GlobalAgents
	if globalAgents == nil {
		globalAgents = &atomic.Int64{}
	}

	provCtx := providerContext(cfg.Providers, defaultName)

	history := cfg.History
	if len(history) == 0 {
		systemPrompt, err := instructions.BuildSystemPrompt(cfg.Cwd, provCtx)
		if err != nil {
			return nil, fmt.Errorf("build system prompt: %w", err)
		}
		if note := environmentNote(cfg.Cwd, time.Now(), cfg.IsolationNote, cfg.RestrictedRoots); note != "" {
			systemPrompt = systemPrompt + "\n\n" + note
		}
		if note := gitAttributionNote(cfg.GitAttribution, cfg.GitAttributionName); note != "" {
			systemPrompt = systemPrompt + "\n\n" + note
		}
		if note := additionalDirsNote(cfg.AdditionalDirectories); note != "" {
			systemPrompt = systemPrompt + "\n\n" + note
		}
		if extra := strings.TrimSpace(cfg.ExtraSystemPrompt); extra != "" {
			systemPrompt = systemPrompt + "\n\n" + extra
		}
		history = []llm.Message{{Role: "system", Content: systemPrompt}}
	}

	defaultProvider := cfg.Providers[defaultName]

	pruneCfg := cfg.PruneCfg
	// Treat zero-value as "use defaults" — Resolve() on an empty
	// ContextPruneConfig produces the in-code defaults.
	if pruneCfg.StaleAfter == 0 && pruneCfg.OutputCapDefault == 0 {
		pruneCfg = (config.ContextPruneConfig{}).Resolve()
	}

	ac := &tools.AgentContext{
		Cwd:                cfg.Cwd,
		SessionID:          cfg.SessionID,
		Logger:             slog.Default(),
		ReadSet:            make(map[string]bool),
		Bus:                cfg.Bus,
		Permissions:        cfg.Perms,
		MaxTurns:           maxTurns,
		Provider:           defaultProvider,
		Providers:          cfg.Providers,
		Registry:           cfg.Registry,
		Depth:              cfg.Depth,
		MaxDepth:           maxDepth,
		GlobalAgents:       globalAgents,
		MaxAgents:          maxAgents,
		AgentID:            cfg.AgentID,
		AgentRole:          cfg.AgentRole,
		Transcripts:        cfg.Transcripts,
		Writer:             cfg.Writer,
		RestrictedRoots:    cfg.RestrictedRoots,
		FileEditHook:       fileEditHookOf(cfg.Hooks),
		LSPNotifier:        cfg.LSPNotifier,
		WebFetchAllowHosts: cfg.WebFetchAllowHosts,
		Capabilities:       cfg.Capabilities,
		IsolationNote:      cfg.IsolationNote,
		OutputCaps: tools.DefaultOutputCaps{
			Default:       pruneCfg.OutputCapDefault,
			PerTool:       pruneCfg.OutputCapsPerTool,
			MaxBytes:      pruneCfg.OutputMaxBytes,
			MaxLineLength: pruneCfg.OutputMaxLineLength,
		},
		Spill: makeSpillWriter(cfg.SessionID),
	}

	// Seed messageUsage from resume state when available so the first
	// MaybeCompact reads real numbers without waiting for the first
	// new turn.
	mu := map[int]llm.MessageUsage{}
	for k, v := range cfg.MessageUsage {
		if k >= 0 && k < len(history) {
			mu[k] = v
		}
	}

	a := &Agent{
		Providers:              cfg.Providers,
		currentProvider:        defaultProvider,
		History:                history,
		Bus:                    cfg.Bus,
		Registry:               cfg.Registry,
		Perms:                  cfg.Perms,
		AgentCtx:               ac,
		Writer:                 cfg.Writer,
		MaxTurns:               maxTurns,
		Hooks:                  cfg.Hooks,
		pruneCfg:               pruneCfg,
		toolMeta:               map[int]*toolMessageMeta{},
		messageUsage:           mu,
		providerCtx:            provCtx,
		compactionProviderName: cfg.CompactionProvider,
		injectedInstructions:   map[string]struct{}{},
	}
	if cfg.LastUsage != nil {
		lu := *cfg.LastUsage
		a.lastUsage.Store(&lu)
	}
	// Wire the checkpoint seam so the `checkpoint` tool can queue a
	// compaction without the tools package importing internal/agent.
	// Each Agent gets its own AgentContext via New(), so a sub-agent's
	// checkpoint compacts only its own history.
	ac.Checkpoint = a
	ac.InstructionResolver = a
	a.refreshEstimate()
	a.startEventHookFanout()
	return a, nil
}

// startEventHookFanout subscribes to the agent's bus and pumps each
// event through Hooks.OnEvent on a dedicated goroutine. No-op when no
// on_event command is configured. The goroutine exits when the bus
// closes the subscriber channel (which happens never today — buses
// outlive agents — but the goroutine is cheap to leave running for the
// agent's lifetime and unsubscribes on best-effort GC).
//
// Runs OFF the agent loop so a slow on_event hook can't stall the
// agent. The subscriber channel is buffered; bursty events that
// outpace the hook are dropped at the bus's normal slow-consumer
// boundary (logged once per drop interval).
func (a *Agent) startEventHookFanout() {
	if a.Hooks == nil || a.Hooks.OnEventCmd == "" || a.Bus == nil {
		return
	}
	cwd := a.AgentCtx.Cwd
	sessionID := a.AgentCtx.SessionID
	ch := a.Bus.Subscribe(64)
	go func() {
		for evt := range ch {
			a.Hooks.OnEvent(cwd, sessionID, evt)
		}
	}()
}

// ResolveOnRead implements tools.InstructionResolver for contextual
// instruction injection. Walks up from absPath collecting ENSO.md /
// AGENTS.md files that govern the path but weren't already folded
// into the static system prompt (those at or above cfg.Cwd are
// covered there); returns a single newline-joined reminder block
// ready to append to the read tool's LLMOutput.
//
// Per-session dedup: each path that produces a reminder is recorded
// so a second read of the same file (or a sibling under the same
// instruction dir) skips the injection. Survives compaction —
// re-injecting would just bloat context.
//
// Returns "" when there's nothing new to inject, when AgentCtx.Cwd
// is empty (e.g. a sub-agent without a cwd), or when the resolver
// surfaces an error (silently swallowed — instructions are
// best-effort enrichment, never a hard failure).
func (a *Agent) ResolveOnRead(absPath string) string {
	if absPath == "" || a.AgentCtx == nil || a.AgentCtx.Cwd == "" {
		return ""
	}
	layers, err := instructions.ResolveForPath(absPath, a.AgentCtx.Cwd)
	if err != nil || len(layers) == 0 {
		return ""
	}

	a.injectedInstructionsMu.Lock()
	defer a.injectedInstructionsMu.Unlock()

	var fresh []instructions.Layer
	for _, l := range layers {
		if _, seen := a.injectedInstructions[l.Name]; seen {
			continue
		}
		fresh = append(fresh, l)
		a.injectedInstructions[l.Name] = struct{}{}
	}
	if len(fresh) == 0 {
		return ""
	}

	var b strings.Builder
	for i, l := range fresh {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "<system-reminder>\nDirectory-scoped instructions from %s — these govern files under that directory:\n\n%s\n</system-reminder>",
			l.Name, l.Body)
	}
	return b.String()
}

// RunOneShot submits a single user prompt, drives the chat→tool loop until
// quiescent, and returns the text content of the final assistant message.
// Used by the spawn_agent tool to drive a child agent. The supplied ctx is
// honoured as the turn context — cancelling it interrupts the run.
func (a *Agent) RunOneShot(ctx context.Context, prompt string) (string, error) {
	a.appendUserMessage(prompt)
	a.AgentCtx.TurnCount = 0

	a.runUntilQuiescent(ctx)

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	for i := len(a.History) - 1; i >= 0; i-- {
		m := a.History[i]
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			return m.Content, nil
		}
	}
	return "", fmt.Errorf("agent produced no settled assistant reply")
}

// drainInputCh non-blockingly pulls every queued message from inputCh
// and returns the count. Used after a cancelled turn so frustration-
// retry submits don't get processed out of order on the next turn.
// Safe because the caller (Run loop) is between turns and is the only
// reader of the channel.
func drainInputCh(inputCh <-chan string) int {
	n := 0
	for {
		select {
		case <-inputCh:
			n++
		default:
			return n
		}
	}
}

// Cancel interrupts the in-flight turn (if any). The agent stays running and
// can accept the next user message.
func (a *Agent) Cancel() {
	a.mu.Lock()
	cancel := a.curCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Run executes the chat loop: receive user messages, stream assistant responses,
// gate and execute tool calls, then loop until the assistant has no more tool
// calls. The parent ctx survives turn-level cancellations (Cancel()); only a
// shutdown of ctx itself terminates Run.
func (a *Agent) Run(ctx context.Context, inputCh <-chan string) error {
	// Fire the on_session_end hook (if configured) when this top-level
	// loop returns. RunOneShot deliberately does NOT fire — that path
	// is used by subagents and workflow roles, where session-end would
	// be spammy and misleading.
	defer func() {
		if a.Hooks != nil {
			a.Hooks.OnSessionEnd(a.AgentCtx.Cwd, a.AgentCtx.SessionID)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case prompt, ok := <-inputCh:
			if !ok {
				return nil
			}

			turnCtx, cancel := context.WithCancel(ctx)
			a.mu.Lock()
			a.curCancel = cancel
			a.mu.Unlock()

			// Turn-scoped permission grants ("Allow Turn" modal button)
			// reset here, before any tool call in the new turn can run.
			// This is the only safe boundary: sub-agent fan-out and
			// chained tool calls all run within one user-driven turn,
			// so resetting on tool boundaries would expire the grant
			// mid-chain and resetting only on EventAgentIdle would let
			// it survive into the next user message. EventUserMessage
			// fires only from real user input (no synthetic submitters
			// in the codebase), which is what the security caveat in
			// TODO P2 #13 demands.
			if a.Perms != nil {
				a.Perms.ResetTurnAllows()
			}

			a.appendUserMessage(prompt)
			a.Bus.Publish(bus.Event{Type: bus.EventUserMessage, Payload: prompt})
			a.AgentCtx.TurnCount = 0
			a.AgentCtx.RecentUserHint = prompt

			a.runUntilQuiescent(turnCtx)

			// Detect "the user cancelled this turn" before the trailing
			// cancel() unconditionally marks turnCtx done. If the user
			// queued more submits while the turn hung (frustrated retries
			// during a stuck tool call), they'd otherwise land as the
			// next turn after cancel — silently corrupting session
			// history. Drain non-blocking and report the count so the UI
			// can render a notice.
			turnCancelled := turnCtx.Err() != nil

			cancel()
			a.mu.Lock()
			a.curCancel = nil
			a.mu.Unlock()

			if turnCancelled && ctx.Err() == nil {
				if n := drainInputCh(inputCh); n > 0 {
					a.Bus.Publish(bus.Event{Type: bus.EventInputDiscarded, Payload: n})
				}
			}

			// Whole pipeline (LLM completion + any tool-call rounds) is now
			// done — distinct from EventAssistantDone which fires per
			// completion. The TUI gates input-busy and the activity
			// "ready" indicator on this so Ctrl-C between turns still
			// cancels.
			a.Bus.Publish(bus.Event{Type: bus.EventAgentIdle})
		}
	}
}

func (a *Agent) runUntilQuiescent(ctx context.Context) {
	// Consume any one-shot tool restriction set by a skill before its
	// submit. The filtered registry is used for ALL turns within this
	// user-message processing (including any tool-call rounds), and
	// reverts to the full registry on the next user message.
	registry := a.Registry
	if names := a.consumeNextTurnTools(); len(names) > 0 {
		registry = a.Registry.Filter(names)
	}

	for {
		if a.AgentCtx.TurnCount >= a.MaxTurns {
			a.Bus.Publish(bus.Event{
				Type:    bus.EventError,
				Payload: fmt.Errorf("max turns reached (%d)", a.MaxTurns),
			})
			return
		}
		a.AgentCtx.TurnCount++

		if _, err := a.MaybeCompact(ctx); err != nil {
			a.AgentCtx.Logger.Warn("compaction failed", "err", err)
		}

		cont, err := a.turn(ctx, registry)
		if err != nil {
			if ctx.Err() != nil {
				a.Bus.Publish(bus.Event{Type: bus.EventCancelled})
				return
			}
			a.Bus.Publish(bus.Event{Type: bus.EventError, Payload: err})
			return
		}
		if !cont {
			return
		}
	}
}

// turn issues one chat round-trip and executes any returned tool calls.
// `registry` controls both which tool defs the model sees and the lookup
// for tool execution; it is the full agent registry by default but may be
// a filtered view (skill restriction).
func (a *Agent) turn(ctx context.Context, registry *tools.Registry) (bool, error) {
	// Snap the active provider once at the start of the turn — this is
	// what makes mid-stream /model swaps safe: the in-flight turn keeps
	// using the provider it started with even if SetProvider runs.
	p := a.Provider()
	release, err := p.Pool.Acquire(ctx)
	if err != nil {
		return false, fmt.Errorf("acquire pool: %w", err)
	}

	req := llm.ChatRequest{
		Messages:        a.History,
		Tools:           registry.ToolDefs(),
		Temperature:     p.Sampler.Temperature,
		TopK:            p.Sampler.TopK,
		TopP:            p.Sampler.TopP,
		MinP:            p.Sampler.MinP,
		PresencePenalty: p.Sampler.PresencePenalty,
	}

	// Cumulative-spend tracking is now driven by provider-reported
	// usage (EventUsage) post-stream. Backends that don't report usage
	// leave cumIn/cumOut unchanged for the affected turn — acceptable
	// trade-off vs. double-counting against real numbers.

	// D2: log a per-turn prefix-size breakdown so the user (and
	// /prune debugging) can see exactly how much each category is
	// contributing to the total prompt. Goes through slog at debug
	// level — lands in $XDG_STATE_HOME/enso/debug.log when --debug is
	// on, no-ops otherwise.
	if a.AgentCtx.Logger != nil {
		bd := a.PrefixBreakdown()
		a.AgentCtx.Logger.Debug("prefix breakdown",
			"total", bd.Total,
			"system", bd.System,
			"pinned", bd.Pinned,
			"tool_active", bd.ToolActive,
			"tool_stubbed", bd.ToolStubbed,
			"conversation", bd.Conversation,
			"messages", len(req.Messages),
		)
	}

	events, err := p.Client.Chat(ctx, req)
	if err != nil {
		release()
		return false, fmt.Errorf("chat: %w", err)
	}

	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls []llm.ToolCall
	var usage llm.MessageUsage

	for evt := range events {
		switch evt.Type {
		case llm.EventTextDelta:
			content.WriteString(evt.Text)
			a.Bus.Publish(bus.Event{Type: bus.EventAssistantDelta, Payload: evt.Text})
		case llm.EventReasoningDelta:
			// Reasoning is shown live but NOT appended to history.Content —
			// the model is supposed to re-derive its reasoning each turn,
			// and saving it bloats context. Provider-reported reasoning
			// token counts (when supplied) ride on the usage event below.
			reasoning.WriteString(evt.Text)
			a.Bus.Publish(bus.Event{Type: bus.EventReasoningDelta, Payload: evt.Text})
		case llm.EventToolCallComplete:
			toolCalls = append(toolCalls, evt.ToolCalls...)
		case llm.EventUsage:
			usage = evt.Usage
		case llm.EventError:
			release()
			return false, evt.Error
		case llm.EventDone:
			// finalize after the loop
		}
	}
	release()

	contentStr := content.String()
	// Fallback for GGUF chat templates (Qwen3/Hermes-style on llama.cpp)
	// that leak tool calls into the assistant text instead of the
	// structured tool_calls channel. Only when the API channel gave us
	// nothing — a well-behaved provider's structured calls always win.
	if len(toolCalls) == 0 {
		if cleaned, inline := llm.ParseInlineToolCalls(contentStr); len(inline) > 0 {
			contentStr = cleaned
			toolCalls = inline
		}
	}
	// Second fallback: Qwen3 thinking-style templates on llama.cpp wrap the
	// tool call *inside* the reasoning channel, so it never reaches content
	// (and the structured tool_calls channel stays empty too). Recover it
	// from reasoning as a last resort. We discard the cleaned reasoning text
	// either way — reasoning is intentionally never persisted to history.
	if len(toolCalls) == 0 && reasoning.Len() > 0 {
		if _, inline := llm.ParseInlineToolCalls(reasoning.String()); len(inline) > 0 {
			toolCalls = inline
		}
	}

	asst := llm.Message{Role: "assistant", Content: contentStr}
	if len(toolCalls) > 0 {
		asst.ToolCalls = toolCalls
	}

	// Cumulative-spend tracking is driven by provider-reported usage
	// below; nothing to do here.

	// An empty assistant message (no content, no tool calls) means the model
	// produced only reasoning or otherwise emitted nothing the API will
	// accept. Persisting it would make the next turn fail with
	// "Assistant message must contain either 'content' or 'tool_calls'!".
	// Surface a friendly note instead, and end the turn cleanly so the user
	// can try again.
	if asst.Content == "" && len(asst.ToolCalls) == 0 {
		a.Bus.Publish(bus.Event{
			Type: bus.EventError,
			Payload: fmt.Errorf(
				"model produced no visible response (the chat template may be emitting tool calls as inline text — try the Unsloth Qwen3.6 GGUFs and a recent llama.cpp build)",
			),
		})
		return false, nil
	}

	a.appendMessage(asst)
	a.stampUsage(len(a.History)-1, usage)
	a.Bus.Publish(bus.Event{Type: bus.EventAssistantDone})

	if len(toolCalls) == 0 {
		return false, nil
	}

	for _, tc := range toolCalls {
		out, parts, meta := a.executeToolCall(ctx, registry, tc)
		a.appendToolMessage(llm.Message{
			Role:       "tool",
			Name:       tc.Function.Name,
			ToolCallID: tc.ID,
			Content:    out,
			Parts:      parts,
		}, meta)
	}

	return true, nil
}

// appendUserMessage appends a user-role message and bumps the
// session-wide user-turn counter the prune subsystem keys off of.
// All user messages should land here (not appendMessage directly) so
// turn-age accounting stays consistent.
func (a *Agent) appendUserMessage(content string) {
	a.userTurnCounter++
	a.appendMessage(llm.Message{Role: "user", Content: content})
}

// appendMessage persists the message (if a Writer is configured) before
// updating in-memory history. The synchronous persist-before-render order
// keeps a crashed process from losing state observed by the user.
func (a *Agent) appendMessage(msg llm.Message) {
	if a.Writer != nil {
		if err := a.Writer.AppendMessage(msg, a.AgentCtx.AgentID); err != nil {
			a.AgentCtx.Logger.Error("session: append message", "err", err)
		}
	}
	a.History = append(a.History, msg)
	a.refreshEstimate()
}

// executeToolCall gates one call through the permission checker and runs
// it. `registry` is the (possibly filtered) registry for this turn — if
// the model called a tool not in it, we return "unknown tool" so the
// model gets fed back a clear error.
//
// Returns (LLMOutput, Meta). Meta carries the side-channel fields the
// prune subsystem reads (PathsRead/PathsWritten/CacheKey); on error
// paths Meta is the zero value, which the prune layer treats as "no
// pruning hints."
func (a *Agent) executeToolCall(ctx context.Context, registry *tools.Registry, tc llm.ToolCall) (string, []llm.MessagePart, tools.ResultMeta) {
	tool := registry.Get(tc.Function.Name)
	if tool == nil {
		return fmt.Sprintf("error: unknown tool %q", tc.Function.Name), nil, tools.ResultMeta{}
	}

	var args map[string]interface{}
	if tc.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("error: parse arguments: %v", err), nil, tools.ResultMeta{}
		}
	}
	if args == nil {
		args = map[string]interface{}{}
	}

	decision, err := a.Perms.Check(tc.Function.Name, args, a.Bus)
	if err != nil {
		return fmt.Sprintf("error: %v", err), nil, tools.ResultMeta{}
	}
	if decision == permissions.Prompt {
		decision = a.requestPrompt(ctx, tc.Function.Name, args)
	}
	if decision == permissions.Deny {
		a.Bus.Publish(bus.Event{
			Type: bus.EventToolCallEnd,
			Payload: map[string]any{
				"name":   tc.Function.Name,
				"denied": true,
			},
		})
		if a.Writer != nil {
			if err := a.Writer.AppendToolCall(tc.ID, tc.Function.Name, args, "permission denied by user", "", "denied"); err != nil {
				a.AgentCtx.Logger.Error("session: append tool_call (denied)", "err", err)
			}
		}
		return "permission denied by user", nil, tools.ResultMeta{}
	}

	a.Bus.Publish(bus.Event{
		Type: bus.EventToolCallStart,
		Payload: map[string]any{
			"name": tc.Function.Name,
			"id":   tc.ID,
			"args": args,
		},
	})

	a.AgentCtx.CurrentToolID = tc.ID
	result, runErr := tool.Run(ctx, args, a.AgentCtx)
	a.AgentCtx.CurrentToolID = ""

	a.Bus.Publish(bus.Event{
		Type: bus.EventToolCallEnd,
		Payload: map[string]any{
			"name":    tc.Function.Name,
			"id":      tc.ID,
			"result":  result.LLMOutput,
			"display": result.DisplayOutput,
			"error":   runErr,
		},
	})

	if a.Writer != nil {
		status := "ok"
		llmOut := result.LLMOutput
		if runErr != nil {
			status = "error"
			llmOut = fmt.Sprintf("error: %v", runErr)
		}
		if err := a.Writer.AppendToolCall(tc.ID, tc.Function.Name, args, llmOut, result.FullOutput, status); err != nil {
			a.AgentCtx.Logger.Error("session: append tool_call", "err", err)
		}
	}

	if runErr != nil {
		return fmt.Sprintf("error: %v", runErr), nil, tools.ResultMeta{}
	}
	return result.LLMOutput, result.Parts, result.Meta
}

// requestPrompt publishes a permission request and blocks for the user's reply
// (or for the turn context to be cancelled).
func (a *Agent) requestPrompt(ctx context.Context, toolName string, args map[string]interface{}) permissions.Decision {
	respCh := make(chan permissions.Decision, 1)
	a.Bus.Publish(bus.Event{
		Type: bus.EventPermissionRequest,
		Payload: &permissions.PromptRequest{
			ToolName:  toolName,
			Args:      args,
			AgentID:   a.AgentCtx.AgentID,
			AgentRole: a.AgentCtx.AgentRole,
			Respond:   respCh,
		},
	})
	select {
	case d := <-respCh:
		return d
	case <-ctx.Done():
		return permissions.Deny
	}
}

// makeSpillWriter returns a SpillWriter rooted at <state-dir>/truncated
// for the given session, and fires a best-effort sweep of expired
// spill files (7-day TTL) in the background so stale files don't
// accumulate on long-lived hosts. Returns nil if state dir resolution
// fails or session ID is empty — truncateWithRecovery then degrades
// to plain truncation.
func makeSpillWriter(sessionID string) tools.SpillWriter {
	if sessionID == "" {
		return nil
	}
	stateDir, err := paths.StateDir()
	if err != nil || stateDir == "" {
		return nil
	}
	root := filepath.Join(stateDir, "truncated")
	go func() {
		// Best-effort sweep — failure here must not affect agent
		// startup. Log via slog so operators can spot a hung disk.
		if removed, err := tools.SweepSpills(root, tools.DefaultSpillMaxAge); err != nil {
			slog.Warn("spill sweep failed", "root", root, "err", err)
		} else if removed > 0 {
			slog.Debug("spill sweep removed expired files",
				"root", root, "removed", removed)
		}
	}()
	return &tools.FileSpill{Root: root, SessionID: sessionID}
}
