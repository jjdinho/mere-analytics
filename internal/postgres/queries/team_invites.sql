-- Invite tokens follow the same shape as api_tokens: plaintext shared via the
-- URL only, sha256 hex persisted. Consume is a single atomic UPDATE so two
-- concurrent claimants can never both succeed (Issue 14 race test).

-- name: CreateTeamInviteForUser :one
-- Membership-gated: caller must be in the team they're inviting into.
INSERT INTO team_invites (id, team_id, created_by, token_hash, expires_at)
SELECT $1, $2, $3, $4, $5
WHERE EXISTS (
    SELECT 1 FROM team_memberships
    WHERE team_id = $2 AND user_id = $3
)
RETURNING id, team_id, created_by, token_hash, expires_at, consumed_at, consumed_by, created_at;

-- name: GetActiveInviteByHash :one
-- Used by the GET /invites/:t confirmation page; expired or consumed → no row.
SELECT i.id, i.team_id, i.created_by, i.token_hash, i.expires_at,
       i.consumed_at, i.consumed_by, i.created_at,
       t.name AS team_name
FROM team_invites i
JOIN teams t ON t.id = i.team_id
WHERE i.token_hash = $1
  AND i.consumed_at IS NULL
  AND i.expires_at > NOW();

-- name: ConsumeInviteByHash :one
-- Atomic burn. RowsAffected via RETURNING — if 0 rows, the invite was
-- consumed, expired, or never existed; caller maps all three to 404.
UPDATE team_invites
SET consumed_at = NOW(),
    consumed_by = $2
WHERE token_hash = $1
  AND consumed_at IS NULL
  AND expires_at > NOW()
RETURNING id, team_id, created_by, token_hash, expires_at, consumed_at, consumed_by, created_at;
