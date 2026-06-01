-- Retire the secret_api flavour of api_tokens. After this migration the only
-- rows are public_ingest snippet tokens (`mere_pub_…`); /v1/* + /mcp bearer
-- auth is served by oauth_access_tokens (migration 0006).
--
-- Pre-condition: code paths that read/write `kind` are gone (the api_tokens
-- queries were rewritten in lockstep with this migration). No production
-- secret rows exist on this branch.
-- Forward-only: contracting; the down-migration restores the column with the
-- old default + check so a rollback leaves a consistent schema.

DELETE FROM api_tokens WHERE kind = 'secret_api';

ALTER TABLE api_tokens DROP COLUMN kind;
ALTER TABLE api_tokens ALTER COLUMN token_plaintext SET NOT NULL;

DROP INDEX IF EXISTS api_tokens_one_active_public_per_project_idx;
CREATE UNIQUE INDEX api_tokens_one_active_per_project_idx
    ON api_tokens(project_id)
    WHERE revoked_at IS NULL;
