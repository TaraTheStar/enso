// SPDX-License-Identifier: AGPL-3.0-or-later

package session

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/TaraTheStar/enso/internal/paths"
)

// CheckpointStoreDir returns the on-disk root for a session's per-turn
// snapshots ($XDG_STATE_HOME/enso/checkpoints/<sessionID>). Snapshot
// dirs live under it named by the checkpoint seq. Shared by the worker
// (which writes snapshots) and the TUI rewind path (which restores them)
// so the layout has a single source of truth.
func CheckpointStoreDir(sessionID string) (string, error) {
	st, err := paths.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(st, "checkpoints", sessionID), nil
}

// RewindPoint is one selectable /rewind target: a checkpoint seq paired
// with the user-message text of that turn (for the picker preview).
type RewindPoint struct {
	Seq       int
	Preview   string
	CreatedAt time.Time
}

// ListRewindPoints returns the session's checkpoints joined to the
// top-level user message at each checkpoint's seq, in seq order, for the
// /rewind picker. A checkpoint whose message was already truncated away
// (shouldn't normally happen) yields an empty preview rather than being
// dropped.
func ListRewindPoints(s *Store, sessionID string) ([]RewindPoint, error) {
	rows, err := s.DB.Query(
		`SELECT c.seq, COALESCE(m.content, ''), c.created_at
		 FROM checkpoints c
		 LEFT JOIN messages m
		   ON m.session_id = c.session_id AND m.seq = c.seq AND m.agent_id = ''
		 WHERE c.session_id = ? ORDER BY c.seq ASC`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list rewind points: %w", err)
	}
	defer rows.Close()
	var out []RewindPoint
	for rows.Next() {
		var p RewindPoint
		var ts int64
		if err := rows.Scan(&p.Seq, &p.Preview, &ts); err != nil {
			return nil, fmt.Errorf("scan rewind point: %w", err)
		}
		p.CreatedAt = time.Unix(ts, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Checkpoint pairs a user-turn message seq with the on-disk filesystem
// snapshot taken just before that turn executed. See the
// 0008_checkpoints.sql migration for the full rationale.
type Checkpoint struct {
	// Seq is the message seq of the user turn this snapshot precedes.
	// Restoring "to this checkpoint" rewinds the conversation to seq-1.
	Seq int
	// Snapshot is the directory name under the session's checkpoint store.
	Snapshot string
	// CreatedAt is when the snapshot was taken.
	CreatedAt time.Time
}

// RecordCheckpoint inserts (or replaces) the checkpoint for the given
// user-turn seq. Replace-on-conflict keeps the row idempotent if a turn
// is re-snapshotted (e.g. a retried submit).
func (w *Writer) RecordCheckpoint(seq int, snapshot string) error {
	if seq <= 0 || snapshot == "" {
		return fmt.Errorf("record checkpoint: invalid seq/snapshot")
	}
	now := time.Now().Unix()
	_, err := w.db.Exec(
		`INSERT INTO checkpoints(session_id, seq, snapshot, created_at) VALUES(?,?,?,?)
		 ON CONFLICT(session_id, seq) DO UPDATE SET snapshot=excluded.snapshot, created_at=excluded.created_at`,
		w.sessionID, seq, snapshot, now,
	)
	if err != nil {
		return fmt.Errorf("record checkpoint: %w", err)
	}
	return nil
}

// ListCheckpoints returns the session's checkpoints in seq order
// (oldest first).
func ListCheckpoints(s *Store, sessionID string) ([]Checkpoint, error) {
	rows, err := s.DB.Query(
		`SELECT seq, snapshot, created_at FROM checkpoints
		 WHERE session_id = ? ORDER BY seq ASC`, sessionID,
	)
	if err != nil {
		return nil, fmt.Errorf("list checkpoints: %w", err)
	}
	defer rows.Close()

	var out []Checkpoint
	for rows.Next() {
		var c Checkpoint
		var ts int64
		if err := rows.Scan(&c.Seq, &c.Snapshot, &ts); err != nil {
			return nil, fmt.Errorf("scan checkpoint: %w", err)
		}
		c.CreatedAt = time.Unix(ts, 0)
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCheckpointsAfter removes every checkpoint with seq > maxSeq and
// returns the snapshot ids it freed, so the caller can remove the
// corresponding on-disk snapshot directories (the FK CASCADE can't reach
// the filesystem). Used by a rewind: restoring to checkpoint K keeps
// seq <= K-1, so the turns at K and later — and their snapshots — no
// longer exist.
func DeleteCheckpointsAfter(s *Store, sessionID string, maxSeq int) ([]string, error) {
	freed, err := snapshotsWhere(s.DB, sessionID, "seq > ?", maxSeq)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(
		`DELETE FROM checkpoints WHERE session_id = ? AND seq > ?`, sessionID, maxSeq,
	); err != nil {
		return nil, fmt.Errorf("delete checkpoints: %w", err)
	}
	return freed, nil
}

// PruneCheckpoints retains the `keep` most recent checkpoints (highest
// seq) and deletes the rest, returning the freed snapshot ids for disk
// cleanup. keep <= 0 deletes all. Bounds per-session snapshot disk use.
func PruneCheckpoints(s *Store, sessionID string, keep int) ([]string, error) {
	if keep < 0 {
		keep = 0
	}
	// The cutoff is the seq of the keep-th newest checkpoint; anything
	// strictly older is pruned. A subquery keeps it one round-trip.
	freed, err := snapshotsWhere(s.DB, sessionID,
		`seq NOT IN (SELECT seq FROM checkpoints WHERE session_id = ? ORDER BY seq DESC LIMIT ?)`,
		sessionID, keep,
	)
	if err != nil {
		return nil, err
	}
	if _, err := s.DB.Exec(
		`DELETE FROM checkpoints WHERE session_id = ?
		   AND seq NOT IN (SELECT seq FROM checkpoints WHERE session_id = ? ORDER BY seq DESC LIMIT ?)`,
		sessionID, sessionID, keep,
	); err != nil {
		return nil, fmt.Errorf("prune checkpoints: %w", err)
	}
	return freed, nil
}

// CaptureCheckpoint snapshots the workspace for one user turn and records
// the checkpoint, then prunes to the `retain` most recent (removing the
// freed snapshot dirs from disk). The snapshot itself is delegated to
// snapFn(ctx, dst) so callers supply the right source — the LOCAL worker
// passes the real project tree; the HOST passes an isolated backend's
// overlay `merged` dir. Centralizing the on-disk layout + record + prune
// + cleanup here keeps those four concerns from drifting between the two
// call sites. Best-effort by contract: the returned error is for the
// caller to log; checkpointing is recovery/observability, never
// load-bearing for the turn.
func CaptureCheckpoint(store *Store, w *Writer, seq, retain int, snapFn func(ctx context.Context, dst string) error) error {
	if w == nil || seq <= 0 {
		return fmt.Errorf("capture checkpoint: invalid writer/seq")
	}
	base, err := CheckpointStoreDir(w.sessionID)
	if err != nil {
		return fmt.Errorf("capture checkpoint: %w", err)
	}
	name := strconv.Itoa(seq)
	dir := filepath.Join(base, name)
	if err := snapFn(context.Background(), dir); err != nil {
		return fmt.Errorf("capture checkpoint: snapshot: %w", err)
	}
	if err := w.RecordCheckpoint(seq, name); err != nil {
		_ = os.RemoveAll(dir) // don't leave an unreferenced snapshot
		return fmt.Errorf("capture checkpoint: record: %w", err)
	}
	freed, err := PruneCheckpoints(store, w.sessionID, retain)
	if err != nil {
		return fmt.Errorf("capture checkpoint: prune: %w", err)
	}
	for _, id := range freed {
		_ = os.RemoveAll(filepath.Join(base, id))
	}
	return nil
}

// SweepCheckpoints reclaims on-disk per-turn snapshot dirs the DB no
// longer references: whole session dirs whose session row is gone (a
// discarded session — the FK CASCADE drops the checkpoint rows but can't
// reach the filesystem), plus orphan <seq> subdirs of a live session with
// no matching checkpoint row (e.g. a crash between snapshot and record).
// olderThan, when > 0, restricts removal to entries whose mtime is at
// least that old, so a snapshot mid-capture is never yanked. Returns the
// count of removed snapshot dirs. Best-effort: a per-entry failure is
// skipped, never fatal. Invoked by `enso prune`.
func SweepCheckpoints(olderThan time.Duration) (int, error) {
	st, err := paths.StateDir()
	if err != nil {
		return 0, err
	}
	root := filepath.Join(st, "checkpoints")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("sweep checkpoints: %w", err)
	}
	store, err := Open()
	if err != nil {
		return 0, fmt.Errorf("sweep checkpoints: open store: %w", err)
	}
	defer store.Close()

	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sid := e.Name()
		sdir := filepath.Join(root, sid)

		var one int
		switch err := store.DB.QueryRow(`SELECT 1 FROM sessions WHERE id = ?`, sid).Scan(&one); err {
		case sql.ErrNoRows:
			// Orphaned: the session is gone, so the whole dir is garbage.
			if olderThan > 0 && !olderThanMtime(sdir, olderThan) {
				continue
			}
			if os.RemoveAll(sdir) == nil {
				removed++
			}
			continue
		case nil:
			// Live session: drop only <seq> dirs with no checkpoint row.
		default:
			continue // DB error: skip this entry, don't risk a live dir
		}

		cps, err := ListCheckpoints(store, sid)
		if err != nil {
			continue
		}
		referenced := make(map[string]bool, len(cps))
		for _, c := range cps {
			referenced[c.Snapshot] = true
		}
		subs, _ := os.ReadDir(sdir)
		for _, sub := range subs {
			if !sub.IsDir() || referenced[sub.Name()] {
				continue
			}
			seqDir := filepath.Join(sdir, sub.Name())
			if olderThan > 0 && !olderThanMtime(seqDir, olderThan) {
				continue
			}
			if os.RemoveAll(seqDir) == nil {
				removed++
			}
		}
	}
	return removed, nil
}

// olderThanMtime reports whether path's mtime is at least d in the past.
// A stat failure returns false (skip — never remove something we can't
// age-check).
func olderThanMtime(path string, d time.Duration) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) >= d
}

// snapshotsWhere returns the snapshot ids of checkpoints matching the
// given WHERE tail (after `session_id = ? AND`). args are the bind
// values for the tail's placeholders.
func snapshotsWhere(db *sql.DB, sessionID, whereTail string, args ...any) ([]string, error) {
	q := `SELECT snapshot FROM checkpoints WHERE session_id = ? AND ` + whereTail
	rows, err := db.Query(q, append([]any{sessionID}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("select snapshots: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("scan snapshot: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// TruncateAfter deletes every top-level and sub-agent message with
// seq > maxSeq in the session, plus their usage rows, rewinding the
// persisted conversation to maxSeq. The tool_calls and events audit
// logs (independent seq spaces, never loaded into resume history) are
// left intact. After a truncate a fresh AttachWriter re-seeds the
// message seq counter from the new MAX(seq), so subsequent appends
// continue cleanly — callers that re-exec/resume get this for free.
func TruncateAfter(s *Store, sessionID string, maxSeq int) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return fmt.Errorf("truncate: begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`DELETE FROM message_usage WHERE session_id = ? AND seq > ?`, sessionID, maxSeq,
	); err != nil {
		return fmt.Errorf("truncate usage: %w", err)
	}
	if _, err := tx.Exec(
		`DELETE FROM messages WHERE session_id = ? AND seq > ?`, sessionID, maxSeq,
	); err != nil {
		return fmt.Errorf("truncate messages: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, time.Now().Unix(), sessionID,
	); err != nil {
		return fmt.Errorf("truncate: bump ts: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("truncate: commit: %w", err)
	}
	return nil
}
