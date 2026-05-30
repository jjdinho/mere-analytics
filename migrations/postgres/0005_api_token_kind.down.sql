DROP INDEX IF EXISTS api_tokens_one_active_public_per_project_idx;

ALTER TABLE api_tokens
    DROP COLUMN IF EXISTS token_plaintext,
    DROP COLUMN IF EXISTS kind;
