// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"fmt"
	"sort"
)

// ToolCallAccumulator merges streamed tool-call deltas into completed ToolCall records.
type ToolCallAccumulator struct {
	calls map[int]*ToolCall
}

// NewToolCallAccumulator creates a new accumulator.
func NewToolCallAccumulator() *ToolCallAccumulator {
	return &ToolCallAccumulator{
		calls: make(map[int]*ToolCall),
	}
}

// Merge incorporates a single delta chunk into the accumulator.
func (a *ToolCallAccumulator) Merge(delta ChatResponseDelta) error {
	for _, tc := range delta.ToolCalls {
		idx := tc.Index

		call, ok := a.calls[idx]
		if !ok {
			call = &ToolCall{Type: "function"}
			a.calls[idx] = call
		}

		if tc.ID != "" {
			call.ID = tc.ID
		}
		if tc.Function.Name != "" {
			call.Function.Name += tc.Function.Name
		}
		if tc.Function.Arguments != nil {
			args, err := coerceArguments(tc.Function.Arguments)
			if err != nil {
				return fmt.Errorf("merge tool call args: %w", err)
			}
			call.Function.Arguments += args
		}
	}

	return nil
}

// Finalize returns all completed tool calls, ordered by their streamed
// delta index. The index order is deterministic and matches the order the
// model generated the calls in — which is what the server tokenised and
// cached. Returning in map-iteration order instead (Go randomises it)
// reorders multi-call turns relative to the server's cached KV, so the
// re-serialised assistant message diverges from the prompt cache at that
// turn and forces a full prompt reprocess. Sorting by index keeps the
// llama.cpp prefix cache intact across turns.
func (a *ToolCallAccumulator) Finalize() []ToolCall {
	idxs := make([]int, 0, len(a.calls))
	for idx := range a.calls {
		idxs = append(idxs, idx)
	}
	sort.Ints(idxs)
	result := make([]ToolCall, 0, len(a.calls))
	for _, idx := range idxs {
		call := a.calls[idx]
		if call.ID != "" && call.Function.Name != "" {
			result = append(result, *call)
		}
	}
	return result
}

func coerceArguments(v interface{}) (string, error) {
	switch args := v.(type) {
	case string:
		return args, nil
	case nil:
		return "", nil
	default:
		data, err := json.Marshal(args)
		if err != nil {
			return "", fmt.Errorf("marshal arguments: %w", err)
		}
		return string(data), nil
	}
}
