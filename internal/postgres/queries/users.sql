-- name: CreateUser :one
INSERT INTO users (id, email, password_hash, must_change_password)
VALUES ($1, $2, $3, $4)
RETURNING id, email, password_hash, must_change_password, created_at, updated_at;

-- name: GetUserByEmail :one
SELECT id, email, password_hash, must_change_password, created_at, updated_at
FROM users
WHERE lower(email) = lower($1);

-- name: GetUserByID :one
SELECT id, email, password_hash, must_change_password, created_at, updated_at
FROM users
WHERE id = $1;

-- name: UpdateUserPassword :exec
UPDATE users
SET password_hash = $2,
    must_change_password = $3,
    updated_at = NOW()
WHERE id = $1;
