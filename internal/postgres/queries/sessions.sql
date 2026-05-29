-- name: CreateSession :one
INSERT INTO sessions (id, user_id, expires_at, csrf_token)
VALUES ($1, $2, $3, $4)
RETURNING id, user_id, expires_at, csrf_token, created_at;

-- name: GetSessionWithUser :one
SELECT
    s.id                    AS session_id,
    s.user_id               AS user_id,
    s.expires_at            AS expires_at,
    s.csrf_token            AS csrf_token,
    s.created_at            AS created_at,
    u.email                 AS user_email,
    u.must_change_password  AS must_change_password
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.id = $1;

-- name: TouchSession :exec
UPDATE sessions
SET expires_at = $2
WHERE id = $1;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id = $1;

-- name: DeleteExpiredSessions :exec
DELETE FROM sessions WHERE expires_at < NOW();
