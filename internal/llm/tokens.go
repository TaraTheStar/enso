// SPDX-License-Identifier: AGPL-3.0-or-later

package llm

// Estimate returns an approximate token count for a slice of messages.
// Uses a 1-token ≈ 4-characters heuristic.
func Estimate(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += len(m.Content) / 4
		total += len(m.Role) / 4
		if m.ToolCallID != "" {
			total += len(m.ToolCallID) / 4
		}
		for _, tc := range m.ToolCalls {
			total += len(tc.ID) / 4
			total += len(tc.Function.Name) / 4
			total += len(tc.Function.Arguments) / 4
		}
	}
	return total
}
