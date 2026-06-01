-- Public ingest token storage. Only one flavour left: the `mere_pub_…`
-- snippet token that lives in client HTML. token_plaintext is intentionally
-- persisted because the project page re-displays it on every visit; the
-- partial unique index api_tokens_one_active_per_project_idx (migration 0007)
-- enforces at most one active token per project so rotation = insert new +
-- revoke old in one tx.
--
-- /v1/* + /mcp bearer auth has moved to oauth_access_tokens (migration 0006);
-- secret_api tokens were retired with the api_tokens.kind column.

-- name: GetPublicTokenForProjectForUser :one
-- Membership-gated read: only succeeds when the viewer is a member of the
-- project's team and the project is not soft-deleted. Plaintext is returned
-- because this token is public by design.
SELECT t.id, t.project_id, t.name, t.token_hash, t.created_at, t.revoked_at, t.token_plaintext
FROM api_tokens t
JOIN projects p ON p.id = t.project_id
JOIN team_memberships tm ON tm.team_id = p.team_id
WHERE t.project_id = $1
  AND tm.user_id = $2
  AND t.revoked_at IS NULL
  AND p.deleted_at IS NULL
LIMIT 1;

-- name: InsertPublicAPIToken :exec
-- Bootstraps the project's public_ingest token. Called from inside the
-- project-create transaction (auth.Service.createProjectWithPublicToken), so
-- no membership EXISTS guard is needed — the project row was just inserted
-- in the same tx by a query that already enforced membership.
INSERT INTO api_tokens (id, project_id, name, token_hash, token_plaintext)
VALUES ($1, $2, $3, $4, $5);

-- name: GetActiveIngestTokenByHash :one
-- Resolves a mere_pub_* token hash to its project. Excludes soft-deleted
-- projects so a deleted project can't keep accepting writes. Used by the
-- ingest path's requirePublicToken middleware; the caller has already
-- verified the PublicTokenPrefix so a non-prefix bearer never reaches here.
SELECT t.id, t.project_id
FROM api_tokens t
JOIN projects p ON p.id = t.project_id
WHERE t.token_hash = $1
  AND t.revoked_at IS NULL
  AND p.deleted_at IS NULL;
