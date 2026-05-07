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
	Pool          *Pool
}

// BuildProviders constructs every Provider in cfg, keyed by its
// config-section name. Errors if cfg is empty — every enso entry
// point requires at least one configured endpoint.
func BuildProviders(cfg map[string]config.ProviderConfig) (map[string]*Provider, error) {
	if len(cfg) == 0 {
		return nil, fmt.Errorf("no providers configured")
	}
	out := make(map[string]*Provider, len(cfg))
	for name, pcfg := range cfg {
		out[name] = NewProvider(name, pcfg)
	}
	return out, nil
}

// NewProvider creates a Provider from config.
func NewProvider(name string, cfg config.ProviderConfig) *Provider {
	concurrency := cfg.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}

	return &Provider{
		Name:          name,
		Client:        &Client{Endpoint: cfg.Endpoint, APIKey: cfg.APIKey, Model: cfg.Model},
		Model:         cfg.Model,
		ContextWindow: cfg.ContextWindow,
		Sampler:       cfg.Sampler,
		Pool:          NewPool(concurrency),
	}
}
