-- OAuth credentials for subscription-backed LLM providers (chatgpt, anthropic).
-- Stored per provider account (not per llm_profile) so multiple profiles share
-- one rotating refresh token safely.
CREATE TABLE IF NOT EXISTS oauth_credentials (
    provider      TEXT PRIMARY KEY,
    client_id     TEXT NOT NULL DEFAULT '',
    access_token  TEXT NOT NULL DEFAULT '',
    refresh_token TEXT NOT NULL DEFAULT '',
    id_token      TEXT NOT NULL DEFAULT '',
    account_hint  TEXT NOT NULL DEFAULT '',
    scopes        TEXT NOT NULL DEFAULT '',
    expires_at    BIGINT NOT NULL DEFAULT 0,
    updated_at    BIGINT NOT NULL DEFAULT 0
);

-- Allow OAuth (chatgpt, anthropic-oauth) and OpenCode (opencode-zen, opencode-go) profile formats.
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_format_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_format_check
    CHECK (format IN ('openai','anthropic','openai-responses','ollama','chatgpt','anthropic-oauth','opencode-zen','opencode-go'));
