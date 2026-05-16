// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"fmt"

	"github.com/TaraTheStar/enso/internal/config"
)

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
		prov := NewProvider(name, pcfg, pool, poolName)
		prov.PoolSwapCost = res.Pools[poolName].SwapCost
		out[name] = prov
	}
	return out, nil
}

// NewProvider creates a Provider from config, bound to the given shared
// pool. Callers go through BuildProviders; tests may call directly.
func NewProvider(name string, cfg config.ProviderConfig, pool *Pool, poolName string) *Provider {
	if pool == nil {
		pool = NewPool(1)
	}
	return &Provider{
		Name:             name,
		Client:           &Client{Endpoint: cfg.Endpoint, APIKey: cfg.APIKey, Model: cfg.Model},
		Model:            cfg.Model,
		ContextWindow:    cfg.ContextWindow,
		Sampler:          cfg.Sampler,
		Pool:             pool,
		PoolName:         poolName,
		Description:      cfg.Description,
		IncludeProviders: true,
		InputPrice:       cfg.InputPricePerMillion,
		OutputPrice:      cfg.OutputPricePerMillion,
	}
}
