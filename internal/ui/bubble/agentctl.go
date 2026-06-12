// SPDX-License-Identifier: AGPL-3.0-or-later

package bubble

import (
	"context"
	"time"

	"github.com/TaraTheStar/enso/internal/agent"
	"github.com/TaraTheStar/enso/internal/backend/host"
	"github.com/TaraTheStar/enso/internal/instructions"
	"github.com/TaraTheStar/enso/internal/llm"
)

// agentControl is the surface the slash commands and the Ctrl-Space
// overlay drive. The agent core always runs behind the Backend seam, so
// the only implementation is sessionAgentControl below: a shim over
// host.Session that turns each verb into a control RPC to the worker
// over the Channel (or serves it from the coalesced telemetry snapshot
// / the host's own provider set where no RPC is needed).
type agentControl interface {
	Provider() *llm.Provider
	ProviderCtx() *instructions.ProviderContext
	SetProvider(name string) error
	EstimateTokens() int
	CumulativeInputTokens() int64
	CumulativeOutputTokens() int64
	ContextWindow() int
	CompactionBudget() int
	PrefixBreakdown() agent.PrefixBreakdown
	CompactPreview() agent.CompactPreviewResult
	ForceCompact(ctx context.Context) (bool, error)
	ForcePrune() (stubbed, beforeTokens, afterTokens int)
	SetNextTurnTools(names []string)
}

// controlRPCTimeout bounds the synchronous control RPCs the slash
// commands make to the worker. These calls run inside the Bubble Tea
// Update loop (dispatchSlash is synchronous — see the comment there),
// so an unbounded host.Session.control on a wedged-but-alive worker
// would freeze the entire TUI permanently: Update never returns, no
// input (including quit) is processed. 10s is generous for what are
// in-memory lookups/mutations worker-side (no LLM call — ForceCompact,
// the one genuinely slow verb, takes the caller's ctx and already runs
// off the Update loop in /compact's goroutine).
const controlRPCTimeout = 10 * time.Second

// controlCtx returns the bounded context every sessionAgentControl RPC
// uses. The caller must cancel.
func controlCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), controlRPCTimeout)
}

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
	ctx, cancel := controlCtx()
	defer cancel()
	return s.sess.SetProvider(ctx, name)
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

func (s *sessionAgentControl) CompactionBudget() int {
	return s.sess.Telemetry().CompactionBudget
}

func (s *sessionAgentControl) PrefixBreakdown() agent.PrefixBreakdown {
	ctx, cancel := controlCtx()
	defer cancel()
	bd, err := s.sess.PrefixBreakdown(ctx)
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
	ctx, cancel := controlCtx()
	defer cancel()
	p, err := s.sess.CompactPreview(ctx)
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
	ctx, cancel := controlCtx()
	defer cancel()
	stubbed, before, after, err := s.sess.ForcePrune(ctx)
	if err != nil {
		return 0, 0, 0
	}
	return stubbed, before, after
}

func (s *sessionAgentControl) SetNextTurnTools(names []string) {
	ctx, cancel := controlCtx()
	defer cancel()
	_ = s.sess.SetNextTurnTools(ctx, names)
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
