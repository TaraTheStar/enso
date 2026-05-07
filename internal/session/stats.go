// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/TaraTheStar/enso/internal/llm"
)

// Stats is a summary of activity across all sessions in the store, optionally
// limited by a `since` cut-off (sessions with updated_at < since are skipped).
type Stats struct {
	SessionCount      int
	InterruptedCount  int
	OldestUpdatedAt   time.Time
	NewestUpdatedAt   time.Time
	MessagesByRole    map[string]int
	SessionsByModel   map[string]int
	ToolCallsByName   map[string]ToolCallStats
	ApproxTotalTokens int
}

// ToolCallStats counts how many times a given tool ran and split by status.
type ToolCallStats struct {
	Total  int
	OK     int
	Error  int
	Denied int
}

// ComputeStats walks the store and aggregates into Stats. If `since` is the
// zero time, all sessions are included. Token totals use the same 4-chars-
// per-token heuristic as `llm.Estimate`.
func ComputeStats(s *Store, since time.Time) (Stats, error) {
	st := Stats{
		MessagesByRole:  map[string]int{},
		SessionsByModel: map[string]int{},
		ToolCallsByName: map[string]ToolCallStats{},
	}

	sinceUnix := int64(0)
	if !since.IsZero() {
		sinceUnix = since.Unix()
	}

	rows, err := s.DB.Query(
		`SELECT id, model, updated_at, interrupted FROM sessions
		 WHERE updated_at >= ? ORDER BY updated_at ASC`, sinceUnix,
	)
	if err != nil {
		return st, fmt.Errorf("query sessions: %w", err)
	}
	var sessionIDs []string
	for rows.Next() {
		var id, model string
		var updatedAt int64
		var interrupted int
		if err := rows.Scan(&id, &model, &updatedAt, &interrupted); err != nil {
			rows.Close()
			return st, err
		}
		sessionIDs = append(sessionIDs, id)
		st.SessionCount++
		st.SessionsByModel[model]++
		if interrupted != 0 {
			st.InterruptedCount++
		}
		t := time.Unix(updatedAt, 0)
		if st.OldestUpdatedAt.IsZero() || t.Before(st.OldestUpdatedAt) {
			st.OldestUpdatedAt = t
		}
		if t.After(st.NewestUpdatedAt) {
			st.NewestUpdatedAt = t
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return st, err
	}

	if len(sessionIDs) == 0 {
		return st, nil
	}

	if err := tallyMessages(s.DB, sessionIDs, &st); err != nil {
		return st, err
	}
	if err := tallyToolCalls(s.DB, sessionIDs, &st); err != nil {
		return st, err
	}
	return st, nil
}

func tallyMessages(db *sql.DB, ids []string, st *Stats) error {
	rows, err := db.Query(
		`SELECT role, content, tool_call_id, tool_calls FROM messages
		 WHERE session_id IN (`+placeholders(len(ids))+`)`, anySlice(ids)...,
	)
	if err != nil {
		return fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m llm.Message
		var toolCallsJSON string
		if err := rows.Scan(&m.Role, &m.Content, &m.ToolCallID, &toolCallsJSON); err != nil {
			return err
		}
		if toolCallsJSON != "" {
			_ = json.Unmarshal([]byte(toolCallsJSON), &m.ToolCalls)
		}
		st.MessagesByRole[m.Role]++
		st.ApproxTotalTokens += llm.Estimate([]llm.Message{m})
	}
	return rows.Err()
}

func tallyToolCalls(db *sql.DB, ids []string, st *Stats) error {
	rows, err := db.Query(
		`SELECT name, status FROM tool_calls
		 WHERE session_id IN (`+placeholders(len(ids))+`)`, anySlice(ids)...,
	)
	if err != nil {
		return fmt.Errorf("query tool_calls: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, status string
		if err := rows.Scan(&name, &status); err != nil {
			return err
		}
		s := st.ToolCallsByName[name]
		s.Total++
		switch status {
		case "ok":
			s.OK++
		case "error":
			s.Error++
		case "denied":
			s.Denied++
		}
		st.ToolCallsByName[name] = s
	}
	return rows.Err()
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, '?')
	}
	return string(out)
}

func anySlice(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// SortedToolNames returns tool names ordered by total descending, then name.
func (s Stats) SortedToolNames() []string {
	names := make([]string, 0, len(s.ToolCallsByName))
	for name := range s.ToolCallsByName {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if s.ToolCallsByName[names[i]].Total != s.ToolCallsByName[names[j]].Total {
			return s.ToolCallsByName[names[i]].Total > s.ToolCallsByName[names[j]].Total
		}
		return names[i] < names[j]
	})
	return names
}

// SortedModels returns model names ordered by session count descending.
func (s Stats) SortedModels() []string {
	names := make([]string, 0, len(s.SessionsByModel))
	for name := range s.SessionsByModel {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if s.SessionsByModel[names[i]] != s.SessionsByModel[names[j]] {
			return s.SessionsByModel[names[i]] > s.SessionsByModel[names[j]]
		}
		return names[i] < names[j]
	})
	return names
}
