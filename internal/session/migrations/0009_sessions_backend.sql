-- Execution provenance: which Backend the session's worker ran behind
-- ("local", "podman", "lima", …). Set host-side at worker attach — at
-- session creation and again on resume, so it records the LATEST
-- backend; per-epoch history lives in the events table as
-- WorkerAttached rows. '' = unknown (pre-provenance rows, or paths
-- that never attach a worker).
ALTER TABLE sessions ADD COLUMN backend TEXT NOT NULL DEFAULT '';
