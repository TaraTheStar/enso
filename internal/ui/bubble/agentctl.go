// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"context"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/instructions"
	"github.com/TaraTheStar/enso/internal/llm"
)

// agentControl is exactly the slice of *agent.Agent the slash commands
// and the Ctrl-Space overlay drive. Naming the surface as an interface
// is what lets the TUI run either in-process (legacy / sandbox-on path,
// where *agent.Agent satisfies it directly) or behind the Backend seam
// (sandbox-off, where a host.Session-backed shim satisfies it). The
// signatures match *agent.Agent verbatim so the in-process path is a
// zero-cost assignment with no behavior change.
type agentControl interface {
	Provider() *llm.Provider
	ProviderCtx() *instructions.ProviderContext
	SetProvider(name string) error
	EstimateTokens() int
	CumulativeInputTokens() int64
	CumulativeOutputTokens() int64
	ContextWindow() int
	PrefixBreakdown() agent.PrefixBreakdown
	CompactPreview() agent.CompactPreviewResult
	ForceCompact(ctx context.Context) (bool, error)
	ForcePrune() (stubbed, beforeTokens, afterTokens int)
	SetNextTurnTools(names []string)
}

// The real agent is the in-process implementation (legacy path).
var _ agentControl = (*agent.Agent)(nil)

// sessionAgentControl adapts a host.Session (the worker behind the
// Backend seam) to agentControl. Mutating / history-reading verbs are
// control RPCs to the worker (where the agent's history lives);
// read-only token+provider numbers come from the coalesced telemetry
// snapshot; provider identity and the "## Available models" context are
// rebuilt from the REAL providers the host already holds (no RPC, and
// behavior-identical because it is the same pure derivation over the
// same provider set the agent uses).
type sessionAgentControl struct {
	sess      *host.Session
	providers map[string]*llm.Provider
}

var _ agentControl = (*sessionAgentControl)(nil)

func (s *sessionAgentControl) Provider() *llm.Provider {
	// The real provider object, looked up by the active name the worker
	// reported. Carries the true ContextWindow + prices the host owns;
	// the worker is credential-scrubbed and could not return one.
	return s.providers[s.sess.Telemetry().Provider]
}

func (s *sessionAgentControl) ProviderCtx() *instructions.ProviderContext {
	return providerContextFrom(s.providers, s.sess.Telemetry().Provider)
}

func (s *sessionAgentControl) SetProvider(name string) error {
	return s.sess.SetProvider(context.Background(), name)
}

func (s *sessionAgentControl) EstimateTokens() int {
	return s.sess.Telemetry().EstTokens
}

func (s *sessionAgentControl) CumulativeInputTokens() int64 {
	return s.sess.Telemetry().CumIn
}

func (s *sessionAgentControl) CumulativeOutputTokens() int64 {
	return s.sess.Telemetry().CumOut
}

func (s *sessionAgentControl) ContextWindow() int {
	return s.sess.Telemetry().ContextWindow
}

func (s *sessionAgentControl) PrefixBreakdown() agent.PrefixBreakdown {
	bd, err := s.sess.PrefixBreakdown(context.Background())
	if err != nil {
		return agent.PrefixBreakdown{}
	}
	return agent.PrefixBreakdown{
		Total:        bd.Total,
		System:       bd.System,
		Pinned:       bd.Pinned,
		ToolActive:   bd.ToolActive,
		ToolStubbed:  bd.ToolStubbed,
		Conversation: bd.Conversation,
	}
}

func (s *sessionAgentControl) CompactPreview() agent.CompactPreviewResult {
	p, err := s.sess.CompactPreview(context.Background())
	if err != nil {
		// Mirror the agent's "nothing to do" so /compact degrades to a
		// harmless message rather than a misleading preview.
		return agent.CompactPreviewResult{NothingToDo: true}
	}
	return agent.CompactPreviewResult{
		BeforeTokens:        p.BeforeTokens,
		EstAfterTokens:      p.EstAfterTokens,
		MessagesToSummarise: p.MessagesToSummarise,
		NothingToDo:         p.NothingToDo,
	}
}

func (s *sessionAgentControl) ForceCompact(ctx context.Context) (bool, error) {
	return s.sess.ForceCompact(ctx)
}

func (s *sessionAgentControl) ForcePrune() (int, int, int) {
	stubbed, before, after, err := s.sess.ForcePrune(context.Background())
	if err != nil {
		return 0, 0, 0
	}
	return stubbed, before, after
}

func (s *sessionAgentControl) SetNextTurnTools(names []string) {
	_ = s.sess.SetNextTurnTools(context.Background(), names)
}

// providerContextFrom mirrors agent.providerContext (unexported): the
// "## Available models" section is a pure function of the provider set
// + active name + the include-providers opt-out. Rebuilding it here is
// behavior-identical and keeps it off the control RPC.
func providerContextFrom(providers map[string]*llm.Provider, active string) *instructions.ProviderContext {
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
