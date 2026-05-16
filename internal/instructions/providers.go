// SPDX-License-Identifier: AGPL-3.0-or-later

package instructions

import (
	"fmt"
	"sort"
	"strings"
)

// ProviderInfo is one configured endpoint as the prompt cares about it.
type ProviderInfo struct {
	Name          string
	Model         string
	ContextWindow int
	Description   string
	// Pool is the shared-hardware pool name (the set of providers
	// sharing a concurrency constraint — e.g. one llama-swap behind one
	// GPU). Rendered inline so the model can see which endpoints
	// contend; omitted when "".
	Pool string
	// SwapCost is the pool's declared swap_cost hint ("low"/"high"/…),
	// empty when unset. Rendered next to the pool name.
	SwapCost string
}

// ProviderContext is the input to the auto-rendered "## Available models"
// section. A nil *ProviderContext (or fewer than two providers) renders
// nothing — single-provider configs gain no value from the section and
// it would only waste tokens.
//
// Active is the provider name the agent is running as at the moment the
// system prompt is built. The section is static for the session: a
// mid-session `/model` swap does not rewrite it, so Active reflects
// session-start (or, for a sub-agent, the model it was spawned with).
// The provider list itself never changes, so the only staleness is the
// single "running as" line (see Agent.providerCtx for why this isn't
// live-updated).
type ProviderContext struct {
	Active    string
	Providers []ProviderInfo
}

// renderProviderSection builds the "## Available models" block, or ""
// when pc is nil or fewer than two providers are configured. Providers
// are listed in name order for deterministic output; the active one is
// called out on its own line and excluded from the "others" list.
func renderProviderSection(pc *ProviderContext) string {
	if pc == nil || len(pc.Providers) < 2 {
		return ""
	}

	provs := make([]ProviderInfo, len(pc.Providers))
	copy(provs, pc.Providers)
	sort.Slice(provs, func(i, j int) bool { return provs[i].Name < provs[j].Name })

	var b strings.Builder
	b.WriteString("\n\n## Available models\n\n")

	for _, p := range provs {
		if p.Name == pc.Active {
			fmt.Fprintf(&b, "You are running as `%s`%s.\n\n", p.Name, attrs(p))
			break
		}
	}

	b.WriteString("Other configured providers (delegate to one with the ")
	b.WriteString("`spawn_agent` tool's `model:` argument, or switch the ")
	b.WriteString("active model with `/model <name>` in the TUI):\n\n")
	for _, p := range provs {
		if p.Name == pc.Active {
			continue
		}
		fmt.Fprintf(&b, "- `%s`%s", p.Name, attrs(p))
		if d := strings.TrimSpace(p.Description); d != "" {
			fmt.Fprintf(&b, " — %s", d)
		}
		b.WriteByte('\n')
	}

	if poolShared(provs) {
		b.WriteString("\nModels sharing a pool run on the same hardware; ")
		b.WriteString("switching the active model between pool-mates forces ")
		b.WriteString("an expensive reload. Prefer finishing work on one ")
		b.WriteString("before `/model`-swapping, and favour delegating to a ")
		b.WriteString("model in a different pool when you need to fan out.\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// poolShared reports whether any non-empty pool has two or more members
// among provs — i.e. there is real shared-hardware contention worth
// warning the model about. All-distinct-pool setups skip the guidance.
func poolShared(provs []ProviderInfo) bool {
	counts := map[string]int{}
	for _, p := range provs {
		if pool := strings.TrimSpace(p.Pool); pool != "" {
			counts[pool]++
			if counts[pool] >= 2 {
				return true
			}
		}
	}
	return false
}

// attrs renders the parenthetical "(model: X, context: Yk, pool: Z)"
// suffix, omitting any field that's unset. Returns "" when nothing is
// known beyond the name.
func attrs(p ProviderInfo) string {
	parts := []string{}
	if m := strings.TrimSpace(p.Model); m != "" && m != p.Name {
		parts = append(parts, "model: "+m)
	}
	if p.ContextWindow > 0 {
		parts = append(parts, fmt.Sprintf("context: %dk", p.ContextWindow/1000))
	}
	if pool := strings.TrimSpace(p.Pool); pool != "" {
		seg := "pool: " + pool
		if sc := strings.TrimSpace(p.SwapCost); sc != "" {
			seg += ", swap-cost: " + sc
		}
		parts = append(parts, seg)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}
