// SPDX-License-Identifier: AGPL-3.0-or-later

package agents

import (
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/tools"
)

// Applied is the trio of runtime values a Spec produces: a possibly-cloned
// provider with sampler overrides, a possibly-filtered registry, and a few
// agent.Config-bound fields the caller threads in directly. Returning a
// struct (rather than mutating provider/registry in place) keeps the Apply
// call non-destructive — the caller's defaults survive untouched if no
// Spec is in play.
type Applied struct {
	Provider     *llm.Provider
	Registry     *tools.Registry
	PromptAppend string
	MaxTurns     int // 0 = leave caller default in place
}

// Apply layers the spec's overrides onto the given baseline. A nil spec is a
// no-op — caller gets back the same provider/registry it passed in. Sampler
// overrides clone the provider so two agents in the same process don't share
// mutated sampler state; the LLM Pool is shared because the endpoint is the
// same.
func Apply(spec *Spec, provider *llm.Provider, registry *tools.Registry) Applied {
	out := Applied{Provider: provider, Registry: registry}
	if spec == nil {
		return out
	}

	if spec.Temperature != nil || spec.TopP != nil || spec.TopK != nil {
		clone := *provider
		if spec.Temperature != nil {
			clone.Sampler.Temperature = *spec.Temperature
		}
		if spec.TopP != nil {
			clone.Sampler.TopP = *spec.TopP
		}
		if spec.TopK != nil {
			clone.Sampler.TopK = *spec.TopK
		}
		out.Provider = &clone
	}

	if len(spec.AllowedTools) > 0 {
		out.Registry = out.Registry.Filter(spec.AllowedTools)
	}
	if len(spec.DeniedTools) > 0 {
		out.Registry = out.Registry.Without(spec.DeniedTools)
	}

	out.PromptAppend = spec.PromptAppend
	out.MaxTurns = spec.MaxTurns
	return out
}
