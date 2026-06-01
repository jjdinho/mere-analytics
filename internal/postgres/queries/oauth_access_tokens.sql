-- Access tokens: opaque random bytes, only sha256 stored. RequireBearer looks
-- the token up by hash and applies the expiry + revocation filters in SQL so
-- the index does the work.

-- name: InsertOAuthAccessToken :exec
INSERT INTO oauth_access_tokens (
    id, token_hash, client_id, user_id, project_id, scope, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetActiveOAuthAccessTokenByHash :one
-- Active = not revoked AND not expired. The unique partial index on
-- (token_hash) WHERE revoked_at IS NULL keeps this on a single index probe.
SELECT id, token_hash, client_id, user_id, project_id, scope,
       expires_at, revoked_at, created_at
FROM oauth_access_tokens
WHERE token_hash = $1
  AND revoked_at IS NULL
  AND expires_at > NOW();

-- name: RevokeOAuthAccessToken :execrows
UPDATE oauth_access_tokens
SET revoked_at = NOW()
WHERE token_hash = $1
  AND revoked_at IS NULL;

-- name: DeleteExpiredOAuthAccessTokens :execrows
-- Called by cmd/maintenance. Sweeps tokens past their expires_at. Revoked-
-- but-not-yet-expired rows are deliberately left alone: the 1h TTL means
-- they vanish on the next sweep anyway, and keeping them preserves the
-- option to surface a revoked-tokens audit view without a schema change.
DELETE FROM oauth_access_tokens
WHERE expires_at < NOW();

-- name: ListActiveAccessTokensForUser :many
-- Future "connected apps" page. Returns the joinable surface (client name +
-- project id + scope + lifecycle timestamps) for the viewer's active grants.
SELECT t.id, t.token_hash, t.client_id, t.user_id, t.project_id, t.scope,
       t.expires_at, t.revoked_at, t.created_at,
       c.name AS client_name
FROM oauth_access_tokens t
JOIN oauth_clients c ON c.id = t.client_id
WHERE t.user_id = $1
  AND t.revoked_at IS NULL
  AND t.expires_at > NOW()
ORDER BY t.created_at DESC;
