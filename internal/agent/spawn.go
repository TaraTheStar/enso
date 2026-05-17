// SPDX-License-Identifier: AGPL-3.0-or-later

package agent

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/TaraTheStar/enso/internal/bus"
	"github.com/TaraTheStar/enso/internal/llm"
	"github.com/TaraTheStar/enso/internal/session"
	"github.com/TaraTheStar/enso/internal/tools"
)

// SpawnTool implements the `spawn_agent` tool. It constructs a child Agent
// that shares the parent's provider, bus, and permissions, runs it on the
// supplied prompt to quiescence, and returns the child's final assistant
// text. Depth and global-agent-count are enforced against the parent's
// AgentContext.
type SpawnTool struct{}

func (SpawnTool) Name() string { return "spawn_agent" }

func (SpawnTool) Description() string {
	return "Run a sub-agent on a focused subtask and return its final answer. " +
		"Use for parallel research, large file digestion, or isolated explorations " +
		"whose intermediate steps shouldn't pollute the main conversation. " +
		"The sub-agent has the same tools by default; pass `tools` to restrict it."
}

func (SpawnTool) Parameters() map[string]interface{} {
	return map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"prompt": map[string]interface{}{
				"type":        "string",
				"description": "What the sub-agent should do.",
			},
			"system": map[string]interface{}{
				"type":        "string",
				"description": "Optional system prompt override (replaces the default three-tier system prompt).",
			},
			"tools": map[string]interface{}{
				"type":        "array",
				"items":       map[string]interface{}{"type": "string"},
				"description": "Optional list of tool names the child is allowed to use. Defaults to the parent's full tool set.",
			},
			"model": map[string]interface{}{
				"type":        "string",
				"description": "Optional provider name from the configured set (e.g. 'qwen-fast', 'qwen-deep'). Defaults to the parent agent's current provider.",
			},
			"role": map[string]interface{}{
				"type":        "string",
				"description": "Optional human-readable label for this sub-agent (e.g. 'reviewer'). Surfaces in permission prompts and the agents pane so the user can tell the children apart.",
			},
		},
		"required": []string{"prompt"},
	}
}

func (SpawnTool) Run(ctx context.Context, args map[string]interface{}, ac *tools.AgentContext) (tools.Result, error) {
	prompt, _ := args["prompt"].(string)
	if prompt == "" {
		return tools.Result{}, fmt.Errorf("spawn_agent: prompt is required")
	}

	if ac.Depth+1 > ac.MaxDepth {
		return tools.Result{
			LLMOutput: fmt.Sprintf("spawn_agent rejected: max recursion depth %d already reached", ac.MaxDepth),
		}, nil
	}
	if ac.GlobalAgents == nil {
		return tools.Result{}, fmt.Errorf("spawn_agent: GlobalAgents counter not configured (parent agent context misconfigured)")
	}

	n := ac.GlobalAgents.Add(1)
	defer ac.GlobalAgents.Add(-1)
	if int(n) > ac.MaxAgents {
		return tools.Result{
			LLMOutput: fmt.Sprintf("spawn_agent rejected: max global agents %d in flight", ac.MaxAgents),
		}, nil
	}

	childRegistry := ac.Registry
	if list, ok := args["tools"].([]interface{}); ok && len(list) > 0 {
		childRegistry = ac.Registry.Filter(asStringSlice(list))
	}

	var history []llm.Message
	if sys, ok := args["system"].(string); ok && sys != "" {
		history = []llm.Message{{Role: "system", Content: sys}}
	}

	agentID := shortUUID()
	// Forward the parent's writer (if any). Sub-agent rows persist with
	// the child's AgentID so transcripts are queryable post-resume via
	// session.LoadAgentTranscript while staying out of top-level Load.
	var childWriter *session.Writer
	if w, ok := ac.Writer.(*session.Writer); ok {
		childWriter = w
	}

	// Per-call provider: child defaults to the parent's current provider
	// (ac.Provider, kept live by Agent.SetProvider). The optional `model`
	// arg picks a different one out of ac.Providers — unknown name is
	// returned to the model as an LLM-visible error so it can correct.
	childDefault := ac.Provider.Name
	if want, _ := args["model"].(string); want != "" {
		if _, ok := ac.Providers[want]; !ok {
			return tools.Result{
				LLMOutput: fmt.Sprintf("spawn_agent: unknown model %q (configured: %v)",
					want, sortedProviderNames(ac.Providers)),
			}, nil
		}
		childDefault = want
	}

	childProviders := ac.Providers
	if childProviders == nil {
		// Defensive: older callers may not have set Providers
		// yet. Fall back to a one-element map containing the parent's
		// current provider so spawn still works.
		childProviders = map[string]*llm.Provider{ac.Provider.Name: ac.Provider}
	}

	roleLabel, _ := args["role"].(string)
	child, err := New(Config{
		Providers:          childProviders,
		DefaultProvider:    childDefault,
		Bus:                ac.Bus,
		Registry:           childRegistry,
		Perms:              ac.Permissions,
		Cwd:                ac.Cwd,
		MaxTurns:           ac.MaxTurns,
		Depth:              ac.Depth + 1,
		MaxDepth:           ac.MaxDepth,
		MaxAgents:          ac.MaxAgents,
		GlobalAgents:       ac.GlobalAgents,
		History:            history,
		AgentID:            agentID,
		AgentRole:          roleLabel,
		Transcripts:        ac.Transcripts,
		Writer:             childWriter,
		WebFetchAllowHosts: ac.WebFetchAllowHosts,
		RestrictedRoots:    ac.RestrictedRoots,
		Capabilities:       ac.Capabilities, // sealed children can still broker
		IsolationNote:      ac.IsolationNote,
	})
	if err != nil {
		return tools.Result{}, fmt.Errorf("spawn_agent: build child: %w", err)
	}

	ac.Bus.Publish(bus.Event{
		Type: bus.EventAgentStart,
		Payload: map[string]any{
			"id":        agentID,
			"parent_id": ac.AgentID,
			"depth":     ac.Depth + 1,
			"prompt":    truncate(prompt, 80),
		},
	})

	text, runErr := child.RunOneShot(ctx, prompt)

	// Capture the completed transcript for click-to-expand in the
	// agents pane. We store regardless of error — partial histories are
	// still useful diagnostically.
	ac.Transcripts.Store(agentID, child.History)

	endPayload := map[string]any{"id": agentID, "parent_id": ac.AgentID}
	if runErr != nil {
		endPayload["error"] = runErr.Error()
	}
	ac.Bus.Publish(bus.Event{Type: bus.EventAgentEnd, Payload: endPayload})

	if runErr != nil {
		return tools.Result{LLMOutput: fmt.Sprintf("subagent error: %v", runErr)}, nil
	}
	return tools.Result{LLMOutput: text, FullOutput: text}, nil
}

// RegisterSpawn adds spawn_agent to the given registry. The caller (top-level
// agent setup, before agent.New) is responsible for invoking this if subagent
// support is desired.
func RegisterSpawn(r *tools.Registry) {
	r.Register(SpawnTool{})
}

// sortedProviderNames returns the configured provider keys in stable
// order for inclusion in the LLM-visible error message when the model
// asks for an unknown one.
func sortedProviderNames(providers map[string]*llm.Provider) []string {
	out := make([]string, 0, len(providers))
	for name := range providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// asStringSlice coerces an unmarshalled JSON array into []string, dropping
// any non-string entries. Used to convert the model's `tools` argument
// (typed as []interface{}) into something Registry.Filter accepts.
func asStringSlice(in []interface{}) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func shortUUID() string {
	id := uuid.NewString()
	if len(id) >= 8 {
		return id[:8]
	}
	return id
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
