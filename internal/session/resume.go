// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/TaraTheStar/enso/internal/llm"
)

// State is the result of resuming a session.
type State struct {
	Info        SessionInfo
	History     []llm.Message
	Interrupted bool // true if the last assistant turn left tool_calls without matching tool messages
}

// Load reads a session and rebuilds its message history. If the session ends
// with assistant tool_calls that lack matching tool replies, synthetic
// "interrupted" tool messages are appended so the model can continue, and
// State.Interrupted is set so the caller can surface a notice.
func Load(s *Store, sessionID string) (*State, error) {
	var info SessionInfo
	var ca, ua int64
	var inter int
	err := s.DB.QueryRow(
		`SELECT id, created_at, updated_at, model, provider, cwd, interrupted, label
		 FROM sessions WHERE id = ?`, sessionID,
	).Scan(&info.ID, &ca, &ua, &info.Model, &info.Provider, &info.Cwd, &inter, &info.Label)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("session %s not found", sessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("load session: %w", err)
	}
	info.Interrupted = inter != 0

	// Top-level resume excludes sub-agent rows (agent_id != '') so the
	// resumed conversation isn't polluted with workflow / spawn_agent
	// transcripts. Use LoadAgentTranscript for those.
	rows, err := s.DB.Query(
		`SELECT role, content, tool_call_id, name, tool_calls
		 FROM messages WHERE session_id = ? AND agent_id = '' ORDER BY seq ASC`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("load messages: %w", err)
	}
	defer rows.Close()

	var history []llm.Message
	for rows.Next() {
		var m llm.Message
		var toolCallsJSON string
		if err := rows.Scan(&m.Role, &m.Content, &m.ToolCallID, &m.Name, &toolCallsJSON); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if toolCallsJSON != "" {
			if err := json.Unmarshal([]byte(toolCallsJSON), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("unmarshal tool_calls: %w", err)
			}
		}
		history = append(history, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	state := &State{Info: info, History: history}

	// Detect interruption: any tool_call without a matching tool message after it.
	state.History, state.Interrupted = backfillInterrupted(state.History)
	return state, nil
}

// LoadAgentTranscript returns the message history for one specific
// sub-agent (or the top-level agent when agentID is ""). Used by future
// "resume an agents-pane transcript view" features and ad-hoc inspection;
// the regular Load only returns top-level rows.
func LoadAgentTranscript(s *Store, sessionID, agentID string) ([]llm.Message, error) {
	rows, err := s.DB.Query(
		`SELECT role, content, tool_call_id, name, tool_calls
		 FROM messages WHERE session_id = ? AND agent_id = ? ORDER BY seq ASC`,
		sessionID, agentID,
	)
	if err != nil {
		return nil, fmt.Errorf("query transcript: %w", err)
	}
	defer rows.Close()

	var history []llm.Message
	for rows.Next() {
		var m llm.Message
		var toolCallsJSON string
		if err := rows.Scan(&m.Role, &m.Content, &m.ToolCallID, &m.Name, &toolCallsJSON); err != nil {
			return nil, fmt.Errorf("scan transcript: %w", err)
		}
		if toolCallsJSON != "" {
			if err := json.Unmarshal([]byte(toolCallsJSON), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("unmarshal tool_calls: %w", err)
			}
		}
		history = append(history, m)
	}
	return history, rows.Err()
}

// backfillInterrupted walks the history; for each assistant message with
// tool_calls, every call must have a matching tool message somewhere after it.
// Missing replies get synthetic "interrupted" tool messages appended at the end
// of history (preserving order). Returns the patched history and whether any
// interruption was found.
func backfillInterrupted(history []llm.Message) ([]llm.Message, bool) {
	answered := map[string]bool{}
	for _, m := range history {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}

	interrupted := false
	patched := append([]llm.Message{}, history...)
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || answered[tc.ID] {
				continue
			}
			interrupted = true
			patched = append(patched, llm.Message{
				Role:       "tool",
				Name:       tc.Function.Name,
				ToolCallID: tc.ID,
				Content:    "tool call interrupted (process exited before completion); user has resumed the session",
			})
			answered[tc.ID] = true
		}
	}
	return patched, interrupted
}
