// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/hooks"
	"github.com/TaraTheStar/enso/internal/instructions"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/permissions"
	"github.com/TaraTheStar/enso/internal/session"
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
	Writer    *session.Writer // optional; nil = ephemeral
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

	// estTokens caches llm.Estimate(History) so the UI goroutine can read
	// the count without racing the agent goroutine's mutations of History.
	// Updated on every appendMessage and after compaction.
	estTokens atomic.Int64

	// cumIn / cumOut accumulate input and output tokens across every
	// chat completion in this session — different from estTokens
	// (which is the *current* context-window usage). Compaction
	// shrinks estTokens but never these two; they reflect spend, not
	// pressure. Heuristic-based (4-char) since OpenAI streaming
	// usage isn't currently parsed; swapping to server-reported
	// counts only requires updating the increment site.
	cumIn  atomic.Int64
	cumOut atomic.Int64
}

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
	a.estTokens.Store(int64(llm.Estimate(a.History)))
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
	Writer    *session.Writer
	History   []llm.Message // optional; if non-nil, replaces the default system prompt
	Cwd       string
	SessionID string
	MaxTurns  int

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

	// Sandbox, when non-nil, is forwarded to AgentContext so the bash
	// tool routes through the container instead of the host.
	Sandbox tools.SandboxRunner

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
}

// New creates an Agent with a system prompt built from the three-tier loader.
// If cfg.History is non-empty it is used verbatim (e.g. when resuming).
func New(cfg Config) (*Agent, error) {
	if len(cfg.Providers) == 0 {
		return nil, fmt.Errorf("agent: at least one provider required")
	}
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

	history := cfg.History
	if len(history) == 0 {
		systemPrompt, err := instructions.BuildSystemPrompt(cfg.Cwd)
		if err != nil {
			return nil, fmt.Errorf("build system prompt: %w", err)
		}
		if note := environmentNote(cfg.Cwd, time.Now(), cfg.Sandbox, cfg.RestrictedRoots); note != "" {
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
		Sandbox:            cfg.Sandbox,
		RestrictedRoots:    cfg.RestrictedRoots,
		FileEditHook:       fileEditHookOf(cfg.Hooks),
		WebFetchAllowHosts: cfg.WebFetchAllowHosts,
	}

	a := &Agent{
		Providers:       cfg.Providers,
		currentProvider: defaultProvider,
		History:         history,
		Bus:             cfg.Bus,
		Registry:        cfg.Registry,
		Perms:           cfg.Perms,
		AgentCtx:        ac,
		Writer:          cfg.Writer,
		MaxTurns:        maxTurns,
		Hooks:           cfg.Hooks,
	}
	a.refreshEstimate()
	return a, nil
}

// RunOneShot submits a single user prompt, drives the chat→tool loop until
// quiescent, and returns the text content of the final assistant message.
// Used by the spawn_agent tool to drive a child agent. The supplied ctx is
// honoured as the turn context — cancelling it interrupts the run.
func (a *Agent) RunOneShot(ctx context.Context, prompt string) (string, error) {
	a.appendMessage(llm.Message{Role: "user", Content: prompt})
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

			a.appendMessage(llm.Message{Role: "user", Content: prompt})
			a.Bus.Publish(bus.Event{Type: bus.EventUserMessage, Payload: prompt})
			a.AgentCtx.TurnCount = 0

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

	// Cumulative-spend tracking (input side): sample what we're sending
	// before the call so a mid-stream cancel still counts the cost. We
	// already paid for the prompt the moment the request hit the wire.
	a.cumIn.Add(int64(llm.Estimate(req.Messages)))

	events, err := p.Client.Chat(ctx, req)
	if err != nil {
		release()
		return false, fmt.Errorf("chat: %w", err)
	}

	var content strings.Builder
	var reasoning strings.Builder
	var toolCalls []llm.ToolCall

	for evt := range events {
		switch evt.Type {
		case llm.EventTextDelta:
			content.WriteString(evt.Text)
			a.Bus.Publish(bus.Event{Type: bus.EventAssistantDelta, Payload: evt.Text})
		case llm.EventReasoningDelta:
			// Reasoning is shown live but NOT appended to history.Content —
			// the model is supposed to re-derive its reasoning each turn,
			// and saving it bloats context. We DO count it for spend
			// tracking though: providers bill for reasoning tokens even
			// when they aren't kept across turns.
			reasoning.WriteString(evt.Text)
			a.Bus.Publish(bus.Event{Type: bus.EventReasoningDelta, Payload: evt.Text})
		case llm.EventToolCallComplete:
			toolCalls = append(toolCalls, evt.ToolCalls...)
		case llm.EventError:
			release()
			return false, evt.Error
		case llm.EventDone:
			// finalize after the loop
		}
	}
	release()

	asst := llm.Message{Role: "assistant", Content: content.String()}
	if len(toolCalls) > 0 {
		asst.ToolCalls = toolCalls
	}

	// Cumulative-spend tracking (output side): the assistant message
	// captures content + tool-call args; reasoning is billed separately
	// because we discard it from history. Using llm.Estimate for the
	// asst message keeps the same heuristic the input side uses.
	a.cumOut.Add(int64(llm.Estimate([]llm.Message{asst})))
	if reasoning.Len() > 0 {
		a.cumOut.Add(int64(reasoning.Len() / 4))
	}

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
	a.Bus.Publish(bus.Event{Type: bus.EventAssistantDone})

	if len(toolCalls) == 0 {
		return false, nil
	}

	for _, tc := range toolCalls {
		out := a.executeToolCall(ctx, registry, tc)
		a.appendMessage(llm.Message{
			Role:       "tool",
			Name:       tc.Function.Name,
			ToolCallID: tc.ID,
			Content:    out,
		})
	}

	return true, nil
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
func (a *Agent) executeToolCall(ctx context.Context, registry *tools.Registry, tc llm.ToolCall) string {
	tool := registry.Get(tc.Function.Name)
	if tool == nil {
		return fmt.Sprintf("error: unknown tool %q", tc.Function.Name)
	}

	var args map[string]interface{}
	if tc.Function.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("error: parse arguments: %v", err)
		}
	}
	if args == nil {
		args = map[string]interface{}{}
	}

	decision, err := a.Perms.Check(tc.Function.Name, args, a.Bus)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
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
		return "permission denied by user"
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
		return fmt.Sprintf("error: %v", runErr)
	}
	return result.LLMOutput
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
