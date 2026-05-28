// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"fmt"
	"time"

	"github.com/TaraTheStar/enso/internal/config"
)

// Defaults for the OpenAI/llama.cpp generation guards. Token counts and
// stall-on-silence are hardware-independent, so these same values are
// correct across a fast GPU and a slow large-context box.
const (
	// defaultMaxTokensCap is the derived output-token ceiling when no
	// explicit max_tokens is set. Chosen to cover near-all legitimate
	// single-turn code edits while still cutting a degeneration loop off
	// long before it fills a large context. Matches OpenAI's gpt-4o cap.
	defaultMaxTokensCap = 16384
	// defaultStallTimeout aborts a stream that produces no token for this
	// long. Generous enough to tolerate prompt-processing pauses and
	// speculative/MTP bursts on a big context.
	defaultStallTimeout = 60 * time.Second
	// defaultRecoverAttempts bounds agent-side auto-recovery per turn.
	defaultRecoverAttempts = 2
)

// resolveMaxTokens picks the output-token cap for the OpenAI/llama.cpp
// path. An explicit MaxTokens wins; otherwise derive a safe default —
// half the context window, capped at defaultMaxTokensCap — so a fresh
// install is protected without truncating legitimate long edits.
func resolveMaxTokens(cfg config.ProviderConfig) int {
	if cfg.MaxTokens > 0 {
		return int(cfg.MaxTokens)
	}
	limit := defaultMaxTokensCap
	if cw := cfg.ContextWindow; cw > 0 && cw/2 < limit {
		limit = cw / 2
	}
	return limit
}

// resolveStallTimeout parses the configured duration; empty = default,
// "0s" disables the watchdog, malformed falls back to the default.
func resolveStallTimeout(g config.GenerationConfig) time.Duration {
	if g.StallTimeout == "" {
		return defaultStallTimeout
	}
	d, err := time.ParseDuration(g.StallTimeout)
	if err != nil || d < 0 {
		return defaultStallTimeout
	}
	return d
}

func resolveRecoverAttempts(g config.GenerationConfig) int {
	if g.MaxRecoverAttempts > 0 {
		return g.MaxRecoverAttempts
	}
	return defaultRecoverAttempts
}

// boolOr returns *p, or def when p is nil (config field unset).
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// Provider wraps one OpenAI-compatible endpoint with its pool and config.
// Client is held as the ChatClient interface so tests can substitute a
// fake via internal/llm/llmtest without standing up a real HTTP server.
type Provider struct {
	Name          string
	Client        ChatClient
	Model         string
	ContextWindow int
	Sampler       config.SamplerConfig
	// Pool bounds concurrency across every provider sharing it. The
	// same *Pool pointer is handed to every co-pooled provider (see
	// BuildProviders) so a /model swap between co-pooled members still
	// contends on one semaphore.
	Pool *Pool
	// PoolName is the resolved pool this provider belongs to. Surfaced
	// in the auto "## Available models" section.
	PoolName string
	// PoolSwapCost is the pool's swap_cost hint ("low"/"high"/…), ""
	// when unset. Surfaced alongside PoolName in the prompt section.
	PoolSwapCost string

	// Description is the capability hint from ProviderConfig, rendered
	// into the "## Available models" prompt section.
	Description string

	// IncludeProviders mirrors the resolved [instructions]
	// include_providers setting. It's a global toggle copied onto every
	// Provider so it survives the config→agent→spawn handoff: sub-agents
	// reuse these pointers and inherit it without extra plumbing.
	// Defaults true (set by NewProvider); the host flips it false when
	// the user opts out.
	IncludeProviders bool

	// InputPrice and OutputPrice are dollars per 1M tokens, copied
	// from ProviderConfig at build time. Zero on both means "free /
	// local model" and the sidebar hides its cost segment.
	InputPrice  float64
	OutputPrice float64

	// AutoRecover and MaxRecoverAttempts drive the agent's belt-and-
	// suspenders recovery from a length-truncated or repetition-aborted
	// turn (see Event.FinishReason). Copied from config so the active
	// provider carries its own policy across a /model swap and into
	// spawned sub-agents. Only the OpenAI/llama.cpp adapter reports the
	// finish reasons that trigger recovery; for others these are inert.
	AutoRecover        bool
	MaxRecoverAttempts int
}

// BuildProviders constructs every Provider in cfg, keyed by its
// config-section name. Errors if cfg is empty — every enso entry point
// requires at least one configured endpoint.
//
// res assigns each provider to a pool and carries the resolved pool
// settings. One *Pool is built per pool name and the SAME pointer is
// shared by every co-pooled provider, so concurrency is enforced
// pool-wide — the fix for shared-hardware contention (parallel calls to
// co-located models fighting over one GPU).
func BuildProviders(cfg map[string]config.ProviderConfig, res config.PoolResolution) (map[string]*Provider, error) {
	if len(cfg) == 0 {
		return nil, fmt.Errorf("no providers configured")
	}

	pools := make(map[string]*Pool, len(res.Pools))
	for name, rp := range res.Pools {
		pools[name] = NewPoolNamed(name, rp.Concurrency, rp.QueueTimeout)
	}

	out := make(map[string]*Provider, len(cfg))
	for name, pcfg := range cfg {
		poolName := res.Assignment[name]
		pool := pools[poolName]
		if pool == nil {
			// Defensive: an assignment with no resolved pool (or no
			// resolution passed at all) gets its own size-1 pool so a
			// provider is never left without a semaphore.
			poolName = "auto-" + name
			pool = NewPoolNamed(poolName, 1, 0)
			pools[poolName] = pool
		}
		prov, err := NewProvider(name, pcfg, pool, poolName)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
		prov.PoolSwapCost = res.Pools[poolName].SwapCost
		out[name] = prov
	}
	return out, nil
}

// NewProvider creates a Provider from config, bound to the given shared
// pool. Callers go through BuildProviders; tests may call directly.
func NewProvider(name string, cfg config.ProviderConfig, pool *Pool, poolName string) (*Provider, error) {
	if pool == nil {
		pool = NewPool(1)
	}
	client, err := newChatClient(cfg)
	if err != nil {
		return nil, err
	}
	return &Provider{
		Name:               name,
		Client:             client,
		Model:              cfg.Model,
		ContextWindow:      cfg.ContextWindow,
		Sampler:            cfg.Sampler,
		Pool:               pool,
		PoolName:           poolName,
		Description:        cfg.Description,
		IncludeProviders:   true,
		InputPrice:         cfg.InputPricePerMillion,
		OutputPrice:        cfg.OutputPricePerMillion,
		AutoRecover:        boolOr(cfg.Generation.AutoRecover, true),
		MaxRecoverAttempts: resolveRecoverAttempts(cfg.Generation),
	}, nil
}

// newChatClient dispatches on cfg.Type to construct the right vendor
// adapter. Empty type means "openai" for back-compat with configs
// written before multi-vendor support — any OpenAI-compat endpoint
// (llama.cpp / vLLM / Groq / OpenRouter / OpenAI proper) falls under
// this case. New types are added here as their adapters land.
func newChatClient(cfg config.ProviderConfig) (ChatClient, error) {
	switch cfg.Type {
	case "", "openai":
		return &OpenAIClient{
			Endpoint:     cfg.Endpoint,
			APIKey:       cfg.APIKey,
			Model:        cfg.Model,
			MaxTokens:    resolveMaxTokens(cfg),
			StallTimeout: resolveStallTimeout(cfg.Generation),
			LoopGuard:    boolOr(cfg.Generation.LoopGuard, true),
		}, nil
	case "bedrock":
		return &BedrockClient{
			Model:                  cfg.Model,
			Region:                 cfg.AWSRegion,
			Profile:                cfg.AWSProfile,
			MaxTokens:              cfg.MaxTokens,
			ExtendedThinking:       cfg.ExtendedThinking,
			ExtendedThinkingBudget: cfg.ExtendedThinkingBudget,
			GuardrailID:            cfg.BedrockGuardrailID,
			GuardrailVersion:       cfg.BedrockGuardrailVersion,
			GuardrailTrace:         cfg.BedrockGuardrailTrace,
			PromptCaching:          cfg.PromptCaching,
		}, nil
	case "vertex":
		return &VertexClient{
			Model:                  cfg.Model,
			Project:                cfg.GCPProject,
			Location:               cfg.GCPLocation,
			MaxTokens:              cfg.MaxTokens,
			ExtendedThinking:       cfg.ExtendedThinking,
			ExtendedThinkingBudget: cfg.ExtendedThinkingBudget,
			Safety:                 cfg.VertexSafety,
		}, nil
	case "anthropic":
		return &AnthropicClient{
			APIKey:                 cfg.APIKey,
			Model:                  cfg.Model,
			BaseURL:                cfg.Endpoint,
			MaxTokens:              cfg.MaxTokens,
			ExtendedThinking:       cfg.ExtendedThinking,
			ExtendedThinkingBudget: cfg.ExtendedThinkingBudget,
			PromptCaching:          cfg.PromptCaching,
		}, nil
	case "anthropic-bedrock":
		return &AnthropicBedrockClient{
			Model:                  cfg.Model,
			Region:                 cfg.AWSRegion,
			Profile:                cfg.AWSProfile,
			MaxTokens:              cfg.MaxTokens,
			ExtendedThinking:       cfg.ExtendedThinking,
			ExtendedThinkingBudget: cfg.ExtendedThinkingBudget,
			GuardrailID:            cfg.BedrockGuardrailID,
			GuardrailVersion:       cfg.BedrockGuardrailVersion,
			GuardrailTrace:         cfg.BedrockGuardrailTrace,
			PromptCaching:          cfg.PromptCaching,
		}, nil
	case "anthropic-vertex":
		return &AnthropicVertexClient{
			Model:                  cfg.Model,
			Region:                 cfg.GCPLocation,
			Project:                cfg.GCPProject,
			MaxTokens:              cfg.MaxTokens,
			ExtendedThinking:       cfg.ExtendedThinking,
			ExtendedThinkingBudget: cfg.ExtendedThinkingBudget,
			PromptCaching:          cfg.PromptCaching,
		}, nil
	default:
		return nil, fmt.Errorf("unknown provider type %q", cfg.Type)
	}
}
