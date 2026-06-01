-- Authorization codes: short-lived (10 min), one-shot. ConsumeCode runs inside
-- a transaction with the access-token insert so a parallel /oauth/token call
-- can never double-spend the code (the UPDATE filters used_at IS NULL).

-- name: InsertOAuthCode :exec
INSERT INTO oauth_codes (
    id, code_hash, client_id, user_id, project_id,
    redirect_uri, scope, code_challenge, code_challenge_method, expires_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ConsumeOAuthCode :one
-- One-shot enforcement: succeeds only when used_at IS NULL AND expires_at >
-- NOW(); a second call against the same code returns pgx.ErrNoRows, which the
-- handler translates to invalid_grant.
UPDATE oauth_codes
SET used_at = NOW()
WHERE code_hash = $1
  AND used_at IS NULL
  AND expires_at > NOW()
RETURNING id, code_hash, client_id, user_id, project_id,
          redirect_uri, scope, code_challenge, code_challenge_method,
          expires_at, used_at, created_at;
