-- Token storage: two flavours sharing one table, distinguished by `kind`.
--
--   kind='public_ingest' (mere_pub_…)  — non-secret snippet token. token_hash
--      indexes the bearer lookup; token_plaintext is also persisted so the
--      project page can re-display it (the snippet needs to be copied many
--      times). One active row per project, enforced by a partial unique
--      index in migration 0005.
--   kind='secret_api' (mere_pat_…)     — pre-existing read/MCP bearer. Only
--      sha256 hex is stored; token_plaintext is NULL. Plaintext is surfaced
--      to the user exactly once via render-on-POST (Issue 3).
--
-- All mutating queries are membership-gated through the project's team_id.
-- ListTokensForProjectForUser returns only secret_api rows — the public token
-- has its own GetPublicTokenForProjectForUser endpoint because the UI shows
-- it in a different surface (always-visible vs. one-shot).

-- name: ListTokensForProjectForUser :many
-- Active secret tokens only. Revoked tokens are retained in the row (for any
-- future audit view) but not shown by default. Public tokens are excluded
-- here; fetch them via GetPublicTokenForProjectForUser.
SELECT t.id, t.project_id, t.name, t.token_hash, t.created_at, t.revoked_at, t.kind, t.token_plaintext
FROM api_tokens t
JOIN projects p ON p.id = t.project_id
JOIN team_memberships tm ON tm.team_id = p.team_id
WHERE t.project_id = $1
  AND tm.user_id = $2
  AND t.kind = 'secret_api'
  AND t.revoked_at IS NULL
  AND p.deleted_at IS NULL
ORDER BY t.created_at ASC;

-- name: GetPublicTokenForProjectForUser :one
-- Returns the project's active public_ingest token (id, plaintext, hash).
-- Plaintext is intentionally fetched: this token is non-secret and the UI
-- displays it verbatim. pgx.ErrNoRows surfaces if the project isn't visible
-- to the viewer OR if it has no active public token (which would be a
-- bootstrap bug — every project should be created with one).
SELECT t.id, t.project_id, t.name, t.token_hash, t.created_at, t.revoked_at, t.kind, t.token_plaintext
FROM api_tokens t
JOIN projects p ON p.id = t.project_id
JOIN team_memberships tm ON tm.team_id = p.team_id
WHERE t.project_id = $1
  AND tm.user_id = $2
  AND t.kind = 'public_ingest'
  AND t.revoked_at IS NULL
  AND p.deleted_at IS NULL
LIMIT 1;

-- name: CreateAPITokenForUser :one
-- Issues a secret_api token. Same WHERE-EXISTS pattern as project create:
-- only succeeds when the caller is a member of the project's team AND the
-- project is not soft-deleted. Public tokens are NOT issued via this path —
-- they are bootstrapped in the project-create tx via InsertPublicAPIToken.
INSERT INTO api_tokens (id, project_id, name, token_hash, kind, token_plaintext)
SELECT $1, $2, $3, $4, 'secret_api', NULL
WHERE EXISTS (
    SELECT 1
    FROM projects p
    JOIN team_memberships tm ON tm.team_id = p.team_id
    WHERE p.id = $2
      AND tm.user_id = $5
      AND p.deleted_at IS NULL
)
RETURNING id, project_id, name, token_hash, created_at, revoked_at, kind, token_plaintext;

-- name: InsertPublicAPIToken :exec
-- Bootstraps the project's public_ingest token. Called from inside the
-- project-create transaction (auth.Service.CreateProjectWithPublicToken), so
-- no membership EXISTS guard is needed — the project row was just inserted
-- in the same tx by a query that already enforced membership. The partial
-- unique index api_tokens_one_active_public_per_project_idx guarantees we
-- can't double-insert (would fail loudly, aborting the tx).
INSERT INTO api_tokens (id, project_id, name, token_hash, kind, token_plaintext)
VALUES ($1, $2, $3, $4, 'public_ingest', $5);

-- name: RevokeAPITokenForUser :execrows
-- Idempotent: WHERE revoked_at IS NULL means a second revoke returns
-- RowsAffected == 0, which the handler translates to 404. Scoped to
-- kind='secret_api' so a stray /revoke on a public token id is a no-op —
-- public-token rotation has its own (future) flow.
UPDATE api_tokens
SET revoked_at = NOW()
WHERE api_tokens.id = $1
  AND api_tokens.project_id = $2
  AND api_tokens.kind = 'secret_api'
  AND api_tokens.revoked_at IS NULL
  AND api_tokens.project_id IN (
      SELECT p.id FROM projects p
      JOIN team_memberships tm ON tm.team_id = p.team_id
      WHERE tm.user_id = $3 AND p.deleted_at IS NULL
  );
