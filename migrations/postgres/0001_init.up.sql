-- All IDs are UUID v7 (time-sortable, RFC 9562), generated app-side via idgen.New().
-- Email case-insensitivity via a lower() functional index (no extension required).

CREATE TABLE users (
    id                   UUID PRIMARY KEY,
    email                TEXT NOT NULL,
    password_hash        TEXT NOT NULL,
    must_change_password BOOLEAN NOT NULL DEFAULT FALSE,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX users_email_lower_idx ON users (lower(email));

CREATE TABLE teams (
    id         UUID PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE team_memberships (
    team_id   UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id   UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (team_id, user_id)
);
CREATE INDEX team_memberships_user_id_idx ON team_memberships(user_id);

CREATE TABLE projects (
    id         UUID PRIMARY KEY,
    team_id    UUID NOT NULL REFERENCES teams(id) ON DELETE RESTRICT,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX projects_team_id_idx ON projects(team_id) WHERE deleted_at IS NULL;

CREATE TABLE api_tokens (
    id         UUID PRIMARY KEY,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    token_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ
);
CREATE INDEX api_tokens_project_id_idx ON api_tokens(project_id) WHERE revoked_at IS NULL;
-- Hot path for bearer auth lookup (step 3+). UNIQUE prevents hash collisions.
CREATE UNIQUE INDEX api_tokens_token_hash_active_idx ON api_tokens(token_hash) WHERE revoked_at IS NULL;

CREATE TABLE sessions (
    id         UUID PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at TIMESTAMPTZ NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX sessions_user_id_idx ON sessions(user_id);
