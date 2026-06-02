// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Fork copies the source session's metadata + ALL top-level messages
// into a new session row and returns the new id. Equivalent to
// ForkAt(s, srcID, -1) — no seq bound. See ForkAt for the full contract.
func Fork(s *Store, srcID string) (string, error) {
	return ForkAt(s, srcID, -1)
}

// ForkAt copies the source session's metadata + its top-level messages
// up to and including seq `maxSeq` into a new session row, re-numbering
// seq from 1, and returns the new id. A maxSeq < 0 means "no bound"
// (copy the whole conversation — the plain Fork). A seq-bounded fork is
// "branch the conversation from message maxSeq": the new session holds
// the prefix, the source keeps its full history.
//
// Tool-call rows are NOT copied: the agent only consults the messages
// table when resuming, and copying rich tool-call output would explode
// storage for forks. Sub-agent transcripts (agent_id != ”) are
// intentionally dropped — a fork is "continue this conversation from
// here", not "branch the entire agent tree". The synthetic/ignored
// flags and reasoning ARE copied so the prefix behaves identically to
// the original (a faithful branch, not a flag-stripped approximation).
// message_usage rows are not copied (cost accounting is per original
// session). The copied session has a fresh created_at/updated_at, an
// empty label (re-derived on the next user message), and is not
// interrupted. It keeps the source's model/provider/cwd so resuming it
// (e.g. `enso --session <new-id>`) reproduces the same environment.
func ForkAt(s *Store, srcID string, maxSeq int) (string, error) {
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
	// from 1, optionally bounded by maxSeq.
	q := `SELECT seq, role, content, tool_call_id, name, tool_calls, synthetic, ignored, reasoning
	      FROM messages WHERE session_id = ? AND agent_id = ''`
	args := []any{srcID}
	if maxSeq >= 0 {
		q += ` AND seq <= ?`
		args = append(args, maxSeq)
	}
	q += ` ORDER BY seq ASC`
	rows, err := tx.Query(q, args...)
	if err != nil {
		return "", fmt.Errorf("fork: load messages: %w", err)
	}
	defer rows.Close()

	insert, err := tx.Prepare(
		`INSERT INTO messages(session_id, seq, role, content, tool_call_id, name, tool_calls, agent_id, synthetic, ignored, reasoning)
		 VALUES(?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?)`,
	)
	if err != nil {
		return "", fmt.Errorf("fork: prepare insert: %w", err)
	}
	defer insert.Close()

	newSeq := 0
	for rows.Next() {
		var srcSeq, synthetic, ignored int
		var role, content, toolCallID, name, toolCalls, reasoning string
		if err := rows.Scan(&srcSeq, &role, &content, &toolCallID, &name, &toolCalls, &synthetic, &ignored, &reasoning); err != nil {
			return "", fmt.Errorf("fork: scan: %w", err)
		}
		newSeq++
		if _, err := insert.Exec(newID, newSeq, role, content, toolCallID, name, toolCalls, synthetic, ignored, reasoning); err != nil {
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
