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
	// MessageUsage carries provider-reported usage for each persisted
	// assistant message, keyed by post-backfill History index. Empty
	// when the session predates real-token-accounting or no usage
	// rows exist for this session.
	MessageUsage map[int]llm.MessageUsage
	// LastUsage is the usage of the most recent assistant message
	// that had a recorded usage row. nil when MessageUsage is empty.
	// Agent.New uses this to seed lastUsage so the first MaybeCompact
	// after resume operates on real numbers.
	LastUsage *llm.MessageUsage
}

// syntheticInterruptedContent is the literal Content used for
// backfill-inserted "tool call interrupted" rows. Recognising it lets
// the usage-row index-mapper skip these (they have no DB seq).
const syntheticInterruptedContent = "tool call interrupted (process exited before completion); user has resumed the session"

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
		`SELECT role, content, tool_call_id, name, tool_calls, synthetic, ignored, reasoning
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
		var synthetic, ignored int
		if err := rows.Scan(&m.Role, &m.Content, &m.ToolCallID, &m.Name, &toolCallsJSON,
			&synthetic, &ignored, &m.Reasoning); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		if toolCallsJSON != "" {
			if err := json.Unmarshal([]byte(toolCallsJSON), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("unmarshal tool_calls: %w", err)
			}
		}
		m.Synthetic = synthetic != 0
		m.Ignored = ignored != 0
		history = append(history, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate messages: %w", err)
	}

	state := &State{Info: info, History: history}

	// Detect interruption: any tool_call without a matching tool message after it.
	state.History, state.Interrupted = backfillInterrupted(state.History)

	// Build a post-backfill seq → History-index map so usage rows
	// (keyed by seq) can be translated to the in-memory History
	// indices the agent uses. Synthetic interrupted-tool rows have
	// no seq; skip them while assigning.
	seqToIdx := make(map[int]int, len(state.History))
	seq := 0
	for i, m := range state.History {
		if m.Role == "tool" && m.Content == syntheticInterruptedContent {
			continue
		}
		seq++
		seqToIdx[seq] = i
	}

	usageRows, err := s.DB.Query(
		`SELECT seq, input_tokens, output_tokens, cache_read_tokens,
		        cache_write_tokens, reasoning_tokens, total_tokens
		 FROM message_usage WHERE session_id = ? AND agent_id = ''
		 ORDER BY seq ASC`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("load message_usage: %w", err)
	}
	defer usageRows.Close()
	for usageRows.Next() {
		var rowSeq int
		var u llm.MessageUsage
		if err := usageRows.Scan(&rowSeq, &u.InputTokens, &u.OutputTokens,
			&u.CacheReadTokens, &u.CacheWriteTokens,
			&u.ReasoningTokens, &u.TotalTokens); err != nil {
			return nil, fmt.Errorf("scan message_usage: %w", err)
		}
		idx, ok := seqToIdx[rowSeq]
		if !ok {
			// Orphan row (its message was deleted, or backfill mismatch).
			// Skip rather than fail — usage is observability, not
			// load-bearing for resume correctness.
			continue
		}
		if state.MessageUsage == nil {
			state.MessageUsage = map[int]llm.MessageUsage{}
		}
		state.MessageUsage[idx] = u
		uCopy := u
		state.LastUsage = &uCopy
	}
	if err := usageRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate message_usage: %w", err)
	}

	return state, nil
}

// LoadAgentTranscript returns the message history for one specific
// sub-agent (or the top-level agent when agentID is ""). Used by future
// "resume an agents-pane transcript view" features and ad-hoc inspection;
// the regular Load only returns top-level rows.
func LoadAgentTranscript(s *Store, sessionID, agentID string) ([]llm.Message, error) {
	rows, err := s.DB.Query(
		`SELECT role, content, tool_call_id, name, tool_calls, synthetic, ignored, reasoning
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
		var synthetic, ignored int
		if err := rows.Scan(&m.Role, &m.Content, &m.ToolCallID, &m.Name, &toolCallsJSON,
			&synthetic, &ignored, &m.Reasoning); err != nil {
			return nil, fmt.Errorf("scan transcript: %w", err)
		}
		if toolCallsJSON != "" {
			if err := json.Unmarshal([]byte(toolCallsJSON), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("unmarshal tool_calls: %w", err)
			}
		}
		m.Synthetic = synthetic != 0
		m.Ignored = ignored != 0
		history = append(history, m)
	}
	return history, rows.Err()
}

// backfillInterrupted walks history and inserts synthetic "interrupted"
// tool messages immediately after each assistant message that has tool_calls
// without matching tool replies. Inline insertion (rather than tail-append)
// is required: the OpenAI chat contract demands a tool reply follow its
// assistant call before any subsequent assistant or user message, and a
// session that resumed after a crash + accepted a new user message before
// being persisted again would otherwise put the synth reply at the wrong
// position on the next reload.
//
// Returns the patched history and whether any interruption was found.
func backfillInterrupted(history []llm.Message) ([]llm.Message, bool) {
	answered := map[string]bool{}
	for _, m := range history {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}

	interrupted := false
	patched := make([]llm.Message, 0, len(history))
	for _, m := range history {
		patched = append(patched, m)
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
				Content:    syntheticInterruptedContent,
				Synthetic:  true,
			})
			answered[tc.ID] = true
		}
	}
	return patched, interrupted
}
