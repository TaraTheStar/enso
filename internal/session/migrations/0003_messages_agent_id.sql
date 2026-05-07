-- Per-message agent attribution. Top-level agent rows have agent_id=''.
-- Sub-agents (spawn_agent, workflow roles) inherit the parent's writer
-- and write rows with their own AgentID populated, so their transcripts
-- are queryable via LoadAgentTranscript without polluting the top-level
-- resume (Load filters WHERE agent_id = '').
ALTER TABLE messages ADD COLUMN agent_id TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_messages_agent ON messages(session_id, agent_id);
