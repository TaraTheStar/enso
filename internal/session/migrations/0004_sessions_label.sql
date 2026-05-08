-- Per-session display label. Auto-derived as a slug from the first
-- top-level user message on append, or set explicitly via /rename.
-- Empty string means "no label yet" — sidebar/picker fall back to id
-- or last-user-message.
ALTER TABLE sessions ADD COLUMN label TEXT NOT NULL DEFAULT '';
