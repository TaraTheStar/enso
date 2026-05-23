-- Per-message Synthetic / Ignored flags.
--
-- `synthetic` = 1 marks a message that was injected programmatically
-- (compaction summaries, env reminders, etc.). Still sent to the model;
-- the agent uses this to detect a prior compaction summary on resume
-- so a second compaction pass can UPDATE it instead of summarizing the
-- summary lossily.
--
-- `ignored` = 1 marks a message present for display/audit only — kept
-- in History but stripped from the outgoing ChatRequest payload.
-- Reserved so future flows can append audit-only rows without polluting
-- the model context.
--
-- Both columns default to 0 so existing rows remain semantically
-- identical (real, sent to model).
ALTER TABLE messages ADD COLUMN synthetic INTEGER NOT NULL DEFAULT 0;
ALTER TABLE messages ADD COLUMN ignored   INTEGER NOT NULL DEFAULT 0;
