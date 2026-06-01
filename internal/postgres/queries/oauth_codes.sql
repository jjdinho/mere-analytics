-- Authorization codes: short-lived (10 min), one-shot. Consumption is a
-- two-step lookup-then-update so the application layer can validate the
-- bound (client_id, redirect_uri, PKCE) tuple BEFORE the row is burnt — a
-- failed PKCE check leaves the code intact for a legitimate retry rather
-- than forcing the user back through /oauth/authorize. One-shot is still
-- enforced by MarkOAuthCodeUsed's `WHERE used_at IS NULL`: a concurrent
-- /oauth/token call racing on the same row produces RowsAffected == 0 on
-- the loser, which the handler maps to invalid_grant.
--
-- expires_at filtering happens in SQL on the lookup so the partial index
-- continues to do the work; the application never sees expired rows.
--
-- This file pairs with internal/oauth/codes.go.

-- name: InsertOAuthCode :exec
INSERT INTO oauth_codes (
    id, code_hash, client_id, user_id, project_id,
    redirect_uri, scope, code_challenge, code_challenge_method, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: GetActiveOAuthCodeByHash :one
-- Read-side of the lookup-then-update. Returns the live row (not used, not
-- expired) so the caller can validate client_id / redirect_uri / PKCE before
-- committing the consume. pgx.ErrNoRows means unknown / expired / already
-- used — all collapse to invalid_grant at the handler.
SELECT id, code_hash, client_id, user_id, project_id,
       redirect_uri, scope, code_challenge, code_challenge_method,
       expires_at, used_at, created_at
FROM oauth_codes
WHERE code_hash = $1
  AND used_at IS NULL
  AND expires_at > NOW();

-- name: MarkOAuthCodeUsed :execrows
-- Write-side of the lookup-then-update. The `used_at IS NULL` predicate is
-- the one-shot guard: a parallel /oauth/token call that also validated the
-- same code races on this UPDATE; exactly one row is touched and the loser
-- sees RowsAffected == 0.
UPDATE oauth_codes
SET used_at = NOW()
WHERE id = $1
  AND used_at IS NULL;
