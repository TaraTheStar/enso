-- Per-message provider-reported token usage. One row per persisted
-- assistant message that had a corresponding EventUsage from the
-- provider. Joined to messages via (session_id, seq, agent_id).
--
-- Cascading delete via the foreign key follows session deletion.
-- ON CONFLICT REPLACE on the composite key keeps re-emissions
-- idempotent (a re-run that emits usage again silently overwrites).

CREATE TABLE IF NOT EXISTS message_usage (
    session_id          TEXT    NOT NULL,
    seq                 INTEGER NOT NULL,
    agent_id            TEXT    NOT NULL DEFAULT '',
    input_tokens        INTEGER NOT NULL DEFAULT 0,
    output_tokens       INTEGER NOT NULL DEFAULT 0,
    cache_read_tokens   INTEGER NOT NULL DEFAULT 0,
    cache_write_tokens  INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens    INTEGER NOT NULL DEFAULT 0,
    total_tokens        INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (session_id, seq, agent_id),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_message_usage_session
    ON message_usage(session_id, agent_id);
