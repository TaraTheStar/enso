// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

import (
	"encoding/json"
	"fmt"
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

// Finalize returns all completed tool calls.
func (a *ToolCallAccumulator) Finalize() []ToolCall {
	result := make([]ToolCall, 0, len(a.calls))
	for _, call := range a.calls {
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
