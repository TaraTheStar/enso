-- Per-message reasoning (chain-of-thought) capture, for REPLAY ONLY.
--
-- `reasoning` holds the assistant turn's streamed chain-of-thought
-- (Qwen3 / DeepSeek-R1 / llama.cpp `--reasoning-budget` style models).
-- It is display-only: persisted so a resumed session / /transcript can
-- replay the thinking the user saw live, but NEVER sent back to the
-- model — the model re-derives its reasoning each turn (mirrors
-- llm.Message.Reasoning's `json:"-"`, which keeps it out of every
-- provider wire shape). Defaults to '' so existing rows are unchanged.
ALTER TABLE messages ADD COLUMN reasoning TEXT NOT NULL DEFAULT '';
