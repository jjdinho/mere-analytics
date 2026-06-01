DROP INDEX IF EXISTS api_tokens_one_active_per_project_idx;

ALTER TABLE api_tokens ALTER COLUMN token_plaintext DROP NOT NULL;
ALTER TABLE api_tokens
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'public_ingest'
        CHECK (kind IN ('public_ingest', 'secret_api'));

CREATE UNIQUE INDEX api_tokens_one_active_public_per_project_idx
    ON api_tokens(project_id)
    WHERE kind = 'public_ingest' AND revoked_at IS NULL;
