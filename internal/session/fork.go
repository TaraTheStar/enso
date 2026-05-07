// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Fork copies the source session's metadata + messages into a new
// session row and returns the new id. Tool-call rows are NOT copied:
// the agent only consults the messages table when resuming, and copying
// rich tool-call output would explode storage for forks. The copied
// session has a fresh created_at/updated_at and is not interrupted.
//
// The new session keeps the source's model/provider/cwd so resuming it
// (e.g. `enso --session <new-id>`) reproduces the same environment.
func Fork(s *Store, srcID string) (string, error) {
	tx, err := s.DB.Begin()
	if err != nil {
		return "", fmt.Errorf("fork: begin tx: %w", err)
	}
	defer tx.Rollback()

	var (
		model, provider, cwd string
	)
	err = tx.QueryRow(
		`SELECT model, provider, cwd FROM sessions WHERE id = ?`, srcID,
	).Scan(&model, &provider, &cwd)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("fork: session %s not found", srcID)
	}
	if err != nil {
		return "", fmt.Errorf("fork: load src: %w", err)
	}

	newID := uuid.NewString()
	now := time.Now().Unix()
	if _, err := tx.Exec(
		`INSERT INTO sessions(id, created_at, updated_at, model, provider, cwd) VALUES(?,?,?,?,?,?)`,
		newID, now, now, model, provider, cwd,
	); err != nil {
		return "", fmt.Errorf("fork: insert new session: %w", err)
	}

	// Copy top-level messages (agent_id = '') in seq order, re-numbering
	// from 1. Sub-agent transcripts are intentionally dropped — a fork
	// is "continue this conversation from here", not "branch the entire
	// agent tree".
	rows, err := tx.Query(
		`SELECT seq, role, content, tool_call_id, name, tool_calls
		 FROM messages WHERE session_id = ? AND agent_id = '' ORDER BY seq ASC`, srcID,
	)
	if err != nil {
		return "", fmt.Errorf("fork: load messages: %w", err)
	}
	defer rows.Close()

	insert, err := tx.Prepare(
		`INSERT INTO messages(session_id, seq, role, content, tool_call_id, name, tool_calls, agent_id)
		 VALUES(?, ?, ?, ?, ?, ?, ?, '')`,
	)
	if err != nil {
		return "", fmt.Errorf("fork: prepare insert: %w", err)
	}
	defer insert.Close()

	newSeq := 0
	for rows.Next() {
		var srcSeq int
		var role, content, toolCallID, name, toolCalls string
		if err := rows.Scan(&srcSeq, &role, &content, &toolCallID, &name, &toolCalls); err != nil {
			return "", fmt.Errorf("fork: scan: %w", err)
		}
		newSeq++
		if _, err := insert.Exec(newID, newSeq, role, content, toolCallID, name, toolCalls); err != nil {
			return "", fmt.Errorf("fork: insert message: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("fork: iterate: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("fork: commit: %w", err)
	}
	return newID, nil
}
