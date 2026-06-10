// SPDX-License-Identifier: AGPL-3.0-or-later

package tools

import (
	"context"
	"strings"
)

// CheckpointRequester is the seam the checkpoint tool uses to ask the
// agent loop to run a compaction pass before the next model completion.
// *agent.Agent satisfies it; defined here so the tools package does not
// import internal/agent.
type CheckpointRequester interface {
	RequestCheckpoint(reason string)
}

// CheckpointTool lets the model declare a logical step boundary
// (typically right after committing the work it just finished) so the
// agent compacts older history before the next chat completion.
//
// The actual compaction is deferred — Run only flips a flag on the
// agent. The next iteration of the agent loop honors the flag in
// MaybeCompact, runs forceCompact with the supplied reason, and the
// model's next completion sees the compacted history.
type CheckpointTool struct{}

func (CheckpointTool) Name() string { return "checkpoint" }

func (CheckpointTool) Description() string {
	return "Mark a logical step boundary. The agent will compact older conversation history before the next model response, replacing it with a summary. Call this last in a step (e.g. right after a successful git commit), not in the middle. Args: reason (optional short string describing what was just completed; included in the compaction event and the summariser's seed)."
}

func (CheckpointTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"reason": map[string]any{
				"type":        "string",
				"description": "Short description of what step was just completed.",
			},
		},
	}
}

func (CheckpointTool) Run(ctx context.Context, args map[string]any, ac *AgentContext) (Result, error) {
	reason, _ := args["reason"].(string)
	reason = strings.TrimSpace(reason)

	if ac == nil || ac.Checkpoint == nil {
		return Result{
			LLMOutput:  "checkpoint: no checkpoint requester wired; compaction not queued",
			FullOutput: "checkpoint: no checkpoint requester wired; compaction not queued",
		}, nil
	}

	ac.Checkpoint.RequestCheckpoint(reason)

	msg := "checkpoint queued: compaction will run before the next model response"
	if reason != "" {
		msg = "checkpoint queued (" + reason + "): compaction will run before the next model response"
	}
	return Result{LLMOutput: msg, FullOutput: msg, DisplayOutput: msg}, nil
}
