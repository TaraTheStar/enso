-- Per-turn checkpoints for /rewind (per-message undo & in-conversation
-- branching).
--
-- A checkpoint pairs a user-turn message `seq` with a filesystem
-- snapshot taken just before that turn executed — i.e. the project
-- state the user was looking at when they sent message `seq`. Restoring
-- to a checkpoint rewinds the conversation to just before that turn
-- (delete messages with seq >= the checkpoint) AND/OR mirrors the
-- snapshot back over the working tree, depending on the /rewind choice.
--
-- `snapshot` is the directory name under the session's checkpoint store
-- ($XDG_STATE_HOME/enso/checkpoints/<session_id>/<snapshot>/); the row
-- and the on-disk snapshot are deleted together (DeleteCheckpointsAfter
-- / PruneCheckpoints return the freed snapshot ids for disk cleanup).
-- ON DELETE CASCADE drops the rows when the session is discarded; the
-- on-disk snapshots are swept separately (the FK can't reach the
-- filesystem) by `enso prune`.
CREATE TABLE IF NOT EXISTS checkpoints (
    session_id TEXT    NOT NULL,
    seq        INTEGER NOT NULL,
    snapshot   TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (session_id, seq),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
