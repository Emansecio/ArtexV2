-- Persist per-profile thinking budget & display preferences (F1/F3) and per-call
-- streaming TTFT in llm_usage (F8). Idempotent for fresh and migrated databases.
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS thinking_budget  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE llm_profiles ADD COLUMN IF NOT EXISTS thinking_display TEXT    NOT NULL DEFAULT '';
ALTER TABLE llm_usage    ADD COLUMN IF NOT EXISTS ttft_ms          INTEGER;
