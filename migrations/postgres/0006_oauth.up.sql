-- OAuth 2.1 authorization server tables (PKCE-only, opaque tokens).
--
--   oauth_clients         registered relying parties (CLI, Claude Code, Cursor)
--   oauth_codes           short-lived (10 min) authorization codes; one-shot
--   oauth_access_tokens   1-hour bearer tokens scoped to one (user, project)
--
-- Storage rules mirror api_tokens / team_invites: only sha256(plaintext) is
-- persisted; plaintext is shown to the client once and discarded server-side.
-- Codes and tokens both carry expires_at so the bearer middleware can short-
-- circuit lookups without touching the issued-at timestamp.
-- Backward-compatible: add-only.

CREATE TABLE oauth_clients (
    id            UUID PRIMARY KEY,
    name          TEXT NOT NULL,
    redirect_uris TEXT[] NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE oauth_codes (
    id                    UUID PRIMARY KEY,
    code_hash             TEXT NOT NULL,
    client_id             UUID NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id               UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id            UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    redirect_uri          TEXT NOT NULL,
    scope                 TEXT NOT NULL,
    code_challenge        TEXT NOT NULL,
    code_challenge_method TEXT NOT NULL,
    expires_at            TIMESTAMPTZ NOT NULL,
    used_at               TIMESTAMPTZ,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX oauth_codes_code_hash_idx ON oauth_codes(code_hash);

CREATE TABLE oauth_access_tokens (
    id         UUID PRIMARY KEY,
    token_hash TEXT NOT NULL,
    client_id  UUID NOT NULL REFERENCES oauth_clients(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    scope      TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX oauth_access_tokens_hash_active_idx
    ON oauth_access_tokens(token_hash)
    WHERE revoked_at IS NULL;
CREATE INDEX oauth_access_tokens_user_id_idx
    ON oauth_access_tokens(user_id)
    WHERE revoked_at IS NULL;
