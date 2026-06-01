-- OAuth client registry: minimal RFC 7591 surface. Public clients only (no
-- client_secret), so the row carries only the identity + the redirect-URI
-- allowlist. Redirect URIs are matched exactly at /oauth/authorize; the
-- application layer applies the additional scheme/host rules (HTTPS or
-- localhost) at registration time.

-- name: InsertOAuthClient :one
INSERT INTO oauth_clients (id, name, redirect_uris)
VALUES ($1, $2, $3)
RETURNING id, name, redirect_uris, created_at;

-- name: GetOAuthClientByID :one
SELECT id, name, redirect_uris, created_at
FROM oauth_clients
WHERE id = $1;
