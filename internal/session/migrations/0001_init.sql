CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    model       TEXT NOT NULL,
    provider    TEXT NOT NULL,
    cwd         TEXT NOT NULL,
    interrupted INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
    session_id     TEXT NOT NULL,
    seq            INTEGER NOT NULL,
    role           TEXT NOT NULL,
    content        TEXT NOT NULL,
    tool_call_id   TEXT NOT NULL DEFAULT '',
    name           TEXT NOT NULL DEFAULT '',
    tool_calls     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (session_id, seq),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS tool_calls (
    session_id   TEXT NOT NULL,
    seq          INTEGER NOT NULL,
    call_id      TEXT NOT NULL,
    name         TEXT NOT NULL,
    args         TEXT NOT NULL DEFAULT '',
    full_output  TEXT NOT NULL DEFAULT '',
    llm_output   TEXT NOT NULL DEFAULT '',
    status       TEXT NOT NULL DEFAULT 'pending',
    PRIMARY KEY (session_id, seq),
    FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id, seq);
CREATE INDEX IF NOT EXISTS idx_tool_calls_session ON tool_calls(session_id, seq);
CREATE INDEX IF NOT EXISTS idx_sessions_updated ON sessions(updated_at DESC);
