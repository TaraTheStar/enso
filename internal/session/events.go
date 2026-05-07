// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/TaraTheStar/enso/internal/llm"
)

// SessionInfo summarises a row in the sessions table.
type SessionInfo struct {
	ID          string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Model       string
	Provider    string
	Cwd         string
	Interrupted bool
}

// Writer persists session state synchronously. Each call commits before
// returning, so callers can publish to the bus immediately afterwards
// (persist-before-render invariant).
type Writer struct {
	db        *sql.DB
	sessionID string
	seq       int // messages table per-session sequence
	toolSeq   int // tool_calls table per-session sequence
	eventSeq  int // events table per-session sequence
	// hasMessages flips true on the first AppendMessage. The host
	// reads it via HasMessages() to decide whether a freshly-created
	// session should be discarded on TUI close (no point keeping a
	// 0-message row that clutters the picker).
	hasMessages bool
}

// NewSession inserts a fresh session row and returns a Writer scoped to it.
func NewSession(s *Store, model, provider, cwd string) (*Writer, error) {
	id := uuid.NewString()
	now := time.Now().Unix()
	_, err := s.DB.Exec(
		`INSERT INTO sessions(id, created_at, updated_at, model, provider, cwd) VALUES(?,?,?,?,?,?)`,
		id, now, now, model, provider, cwd,
	)
	if err != nil {
		return nil, fmt.Errorf("insert session: %w", err)
	}
	return &Writer{db: s.DB, sessionID: id}, nil
}

// AttachWriter returns a Writer for an existing session. It seeds the
// per-table sequences from each table's max existing seq so subsequent
// appends continue cleanly. hasMessages starts true: a session worth
// resuming necessarily has prior content (Load wouldn't have returned
// otherwise), so an empty close shouldn't trigger Discard.
func AttachWriter(s *Store, sessionID string) (*Writer, error) {
	w := &Writer{db: s.DB, sessionID: sessionID, hasMessages: true}
	if err := seedSeq(s.DB, "messages", sessionID, &w.seq); err != nil {
		return nil, fmt.Errorf("attach messages: %w", err)
	}
	if err := seedSeq(s.DB, "tool_calls", sessionID, &w.toolSeq); err != nil {
		return nil, fmt.Errorf("attach tool_calls: %w", err)
	}
	if err := seedSeq(s.DB, "events", sessionID, &w.eventSeq); err != nil {
		return nil, fmt.Errorf("attach events: %w", err)
	}
	return w, nil
}

func seedSeq(db *sql.DB, table, sessionID string, into *int) error {
	var maxSeq sql.NullInt64
	q := fmt.Sprintf(`SELECT MAX(seq) FROM %s WHERE session_id = ?`, table)
	if err := db.QueryRow(q, sessionID).Scan(&maxSeq); err != nil {
		return err
	}
	if maxSeq.Valid {
		*into = int(maxSeq.Int64)
	}
	return nil
}

// SessionID returns this writer's session id.
func (w *Writer) SessionID() string { return w.sessionID }

// AppendMessage records one llm.Message at the next sequence number.
// `agentID` attributes the row to a specific agent; "" means the
// top-level agent (Load filters those for resume). Sub-agents
// (spawn_agent, workflow roles) pass their own id so their transcripts
// stay queryable but don't leak into top-level resume.
func (w *Writer) AppendMessage(msg llm.Message, agentID string) error {
	w.seq++
	toolCallsJSON := ""
	if len(msg.ToolCalls) > 0 {
		b, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return fmt.Errorf("marshal tool_calls: %w", err)
		}
		toolCallsJSON = string(b)
	}
	now := time.Now().Unix()
	tx, err := w.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO messages(session_id, seq, role, content, tool_call_id, name, tool_calls, agent_id)
		 VALUES(?,?,?,?,?,?,?,?)`,
		w.sessionID, w.seq, msg.Role, msg.Content, msg.ToolCallID, msg.Name, toolCallsJSON, agentID,
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("insert message: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, now, w.sessionID,
	); err != nil {
		tx.Rollback()
		return fmt.Errorf("update session ts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	w.hasMessages = true
	return nil
}

// HasMessages reports whether at least one message has been persisted
// to this session.
func (w *Writer) HasMessages() bool { return w.hasMessages }

// Discard deletes the session row (cascading to messages, tool_calls,
// and events via FK constraints). Used by the TUI on close when the
// user opened the app and immediately quit without typing anything —
// keeps the session picker free of zero-message clutter.
func (w *Writer) Discard() error {
	_, err := w.db.Exec(`DELETE FROM sessions WHERE id = ?`, w.sessionID)
	if err != nil {
		return fmt.Errorf("discard session: %w", err)
	}
	return nil
}

// AppendEvent records one bus event into the events audit log. The
// payload is JSON-marshalled for diagnostic replay; transient/in-process
// fields (chans, errors) are stringified by the caller before passing in.
// Distinct seq from messages.seq and tool_calls.seq.
func (w *Writer) AppendEvent(eventType string, payload any) error {
	w.eventSeq++
	var raw []byte
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("marshal event payload: %w", err)
		}
		raw = b
	}
	now := time.Now().Unix()
	_, err := w.db.Exec(
		`INSERT INTO events(session_id, seq, type, payload, ts) VALUES(?,?,?,?,?)`,
		w.sessionID, w.eventSeq, eventType, raw, now,
	)
	if err != nil {
		return fmt.Errorf("insert event: %w", err)
	}
	return nil
}

// AppendToolCall persists one tool execution into the tool_calls table.
// Args is JSON-marshalled; status is one of "ok", "error", "denied".
// Distinct seq from messages.seq.
func (w *Writer) AppendToolCall(callID, name string, args map[string]interface{}, llmOutput, fullOutput, status string) error {
	w.toolSeq++
	argsJSON := ""
	if len(args) > 0 {
		b, err := json.Marshal(args)
		if err != nil {
			return fmt.Errorf("marshal tool args: %w", err)
		}
		argsJSON = string(b)
	}
	_, err := w.db.Exec(
		`INSERT INTO tool_calls(session_id, seq, call_id, name, args, llm_output, full_output, status)
		 VALUES(?,?,?,?,?,?,?,?)`,
		w.sessionID, w.toolSeq, callID, name, argsJSON, llmOutput, fullOutput, status,
	)
	if err != nil {
		return fmt.Errorf("insert tool_call: %w", err)
	}
	return nil
}

// MarkInterrupted flips the session's interrupted flag.
func (w *Writer) MarkInterrupted(v bool) error {
	flag := 0
	if v {
		flag = 1
	}
	_, err := w.db.Exec(`UPDATE sessions SET interrupted = ? WHERE id = ?`, flag, w.sessionID)
	if err != nil {
		return fmt.Errorf("mark interrupted: %w", err)
	}
	return nil
}

// SessionInfoWithStats is a SessionInfo augmented with cheap aggregate
// counts. Same filter convention as resume: only top-level messages
// (agent_id = ”) count, so sub-agent transcripts don't inflate the
// numbers.
type SessionInfoWithStats struct {
	SessionInfo
	MessageCount int
	ApproxTokens int
	// LastUserMessage is the content of the most-recent top-level user
	// turn, surfaced in the splash and session picker so users can
	// identify a session by what they last asked instead of by id.
	// Empty when the session has no user messages yet.
	LastUserMessage string
}

// ListRecentWithStats is ListRecent plus a per-session message count
// and approximate token total. Tokens use the same len(content)/4
// heuristic as llm.Estimate.
func ListRecentWithStats(s *Store, limit int) ([]SessionInfoWithStats, error) {
	// HAVING msg_count > 0 hides any 0-message session: there's nothing
	// to resume from one, and the most common cause is a launch+quit
	// that never reached a real prompt. Going forward those are
	// Discard()ed on close, but historical rows from before that fix
	// still need filtering at the read path.
	rows, err := s.DB.Query(
		`SELECT s.id, s.created_at, s.updated_at, s.model, s.provider, s.cwd, s.interrupted,
		        COUNT(m.seq) AS msg_count,
		        COALESCE(SUM(LENGTH(m.content)), 0) AS char_total,
		        COALESCE((SELECT content FROM messages
		                  WHERE session_id = s.id AND agent_id = '' AND role = 'user'
		                  ORDER BY seq DESC LIMIT 1), '') AS last_user_msg
		 FROM sessions s
		 LEFT JOIN messages m ON m.session_id = s.id AND m.agent_id = ''
		 GROUP BY s.id
		 HAVING msg_count > 0
		 ORDER BY s.updated_at DESC
		 LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions with stats: %w", err)
	}
	defer rows.Close()

	var out []SessionInfoWithStats
	for rows.Next() {
		var info SessionInfoWithStats
		var ca, ua int64
		var inter int
		var charTotal int64
		if err := rows.Scan(
			&info.ID, &ca, &ua, &info.Model, &info.Provider, &info.Cwd, &inter,
			&info.MessageCount, &charTotal, &info.LastUserMessage,
		); err != nil {
			return nil, fmt.Errorf("scan session stats: %w", err)
		}
		info.CreatedAt = time.Unix(ca, 0)
		info.UpdatedAt = time.Unix(ua, 0)
		info.Interrupted = inter != 0
		info.ApproxTokens = int(charTotal / 4)
		out = append(out, info)
	}
	return out, rows.Err()
}

// ListRecent returns the N most-recently-updated sessions.
func ListRecent(s *Store, limit int) ([]SessionInfo, error) {
	rows, err := s.DB.Query(
		`SELECT id, created_at, updated_at, model, provider, cwd, interrupted
		 FROM sessions ORDER BY updated_at DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var out []SessionInfo
	for rows.Next() {
		var info SessionInfo
		var ca, ua int64
		var inter int
		if err := rows.Scan(&info.ID, &ca, &ua, &info.Model, &info.Provider, &info.Cwd, &inter); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		info.CreatedAt = time.Unix(ca, 0)
		info.UpdatedAt = time.Unix(ua, 0)
		info.Interrupted = inter != 0
		out = append(out, info)
	}
	return out, rows.Err()
}
