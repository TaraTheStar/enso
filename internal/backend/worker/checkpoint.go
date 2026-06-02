// SPDX-License-Identifier: AGPL-3.0-or-later

package worker

import (
	"context"
	"log/slog"

	"github.com/TaraTheStar/enso/internal/backend/workspace"
	"github.com/TaraTheStar/enso/internal/session"
)

// newCheckpointFn builds the OnUserTurn callback that snapshots the
// workspace for /rewind on the LOCAL backend, where the worker shares
// the host filesystem (cwd is the real project dir) and owns a real
// session.Writer. Each genuine user turn: snapshot the tree into
// <store>/<seq>, record the checkpoint row, then prune to `retain` most
// recent (removing the freed snapshot dirs from disk).
//
// Everything is BEST-EFFORT: a failure logs and returns without
// disturbing the turn — checkpointing is recovery/observability, never
// load-bearing for the turn itself. Returns nil (checkpointing off) when
// the state dir can't be resolved. The snapshot/record/prune/cleanup
// sequence lives in session.CaptureCheckpoint, shared with the host-side
// isolated path.
func newCheckpointFn(store *session.Store, w *session.Writer, cwd, sessionID string, retain int) func(seq int) {
	if _, err := session.CheckpointStoreDir(sessionID); err != nil {
		slog.Warn("checkpoint: state dir unavailable, per-turn snapshots disabled", "err", err)
		return nil
	}
	return func(seq int) {
		err := session.CaptureCheckpoint(store, w, seq, retain, func(ctx context.Context, dst string) error {
			return workspace.SnapshotTree(ctx, cwd, dst)
		})
		if err != nil {
			slog.Warn("checkpoint: capture failed", "seq", seq, "err", err)
		}
	}
}
