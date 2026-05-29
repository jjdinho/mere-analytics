-- Token storage: plaintext is "mere_pat_" + base64.RawURLEncoding 32 bytes
-- (auth.GenerateToken); only sha256 hex is persisted. Plaintext is shown to
-- the user exactly once via render-on-POST (Issue 3).
--
-- All mutating queries are membership-gated through the project's team_id.

-- name: ListTokensForProjectForUser :many
-- Active tokens only. Revoked tokens are retained in the row (for any future
-- audit view) but not shown by default.
SELECT t.id, t.project_id, t.name, t.token_hash, t.created_at, t.revoked_at
FROM api_tokens t
JOIN projects p ON p.id = t.project_id
JOIN team_memberships tm ON tm.team_id = p.team_id
WHERE t.project_id = $1
  AND tm.user_id = $2
  AND t.revoked_at IS NULL
  AND p.deleted_at IS NULL
ORDER BY t.created_at ASC;

-- name: CreateAPITokenForUser :one
-- Same WHERE-EXISTS pattern as project create: only succeeds when the caller
-- is a member of the project's team AND the project is not soft-deleted.
INSERT INTO api_tokens (id, project_id, name, token_hash)
SELECT $1, $2, $3, $4
WHERE EXISTS (
    SELECT 1
    FROM projects p
    JOIN team_memberships tm ON tm.team_id = p.team_id
    WHERE p.id = $2
      AND tm.user_id = $5
      AND p.deleted_at IS NULL
)
RETURNING id, project_id, name, token_hash, created_at, revoked_at;

-- name: RevokeAPITokenForUser :execrows
-- Idempotent: WHERE revoked_at IS NULL means a second revoke returns
-- RowsAffected == 0, which the handler translates to 404.
UPDATE api_tokens
SET revoked_at = NOW()
WHERE api_tokens.id = $1
  AND api_tokens.project_id = $2
  AND api_tokens.revoked_at IS NULL
  AND api_tokens.project_id IN (
      SELECT p.id FROM projects p
      JOIN team_memberships tm ON tm.team_id = p.team_id
      WHERE tm.user_id = $3 AND p.deleted_at IS NULL
  );
