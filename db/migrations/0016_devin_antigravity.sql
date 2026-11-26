-- Allow Devin (devin) and Antigravity CLI (antigravity-cli) profile formats.
ALTER TABLE llm_profiles DROP CONSTRAINT IF EXISTS llm_profiles_format_check;
ALTER TABLE llm_profiles ADD  CONSTRAINT llm_profiles_format_check
    CHECK (format IN ('openai','anthropic','openai-responses','ollama','chatgpt','anthropic-oauth','opencode-zen','opencode-go','devin','antigravity-cli'));
