-- name: CreateTeam :one
INSERT INTO teams (id, name)
VALUES ($1, $2)
RETURNING id, name, created_at;

-- name: GetTeamByID :one
SELECT id, name, created_at
FROM teams
WHERE id = $1;

-- name: CreateTeamMembership :exec
INSERT INTO team_memberships (team_id, user_id)
VALUES ($1, $2);

-- name: CreateTeamMembershipIfMissing :exec
-- Used by invite-consume and signup-with-invite where "user already in this
-- team" must be silently tolerated (Issue 10). Avoids the 25P02 abort that
-- a raw PK-violation would cause inside a transaction.
INSERT INTO team_memberships (team_id, user_id)
VALUES ($1, $2)
ON CONFLICT (team_id, user_id) DO NOTHING;

-- name: ListTeamsForUser :many
SELECT t.id, t.name, t.created_at
FROM teams t
JOIN team_memberships tm ON tm.team_id = t.id
WHERE tm.user_id = $1
ORDER BY t.created_at ASC;

-- name: GetTeamForUser :one
-- Membership-gated read used by viewer.Teams().ByID(). Zero rows → ErrNotVisible.
SELECT t.id, t.name, t.created_at
FROM teams t
JOIN team_memberships tm ON tm.team_id = t.id
WHERE t.id = $1 AND tm.user_id = $2;

-- name: ListMembersForTeamForUser :many
-- Returns members of $1 if the caller ($2) is themselves a member; otherwise
-- zero rows (which surfaces as ErrNotVisible at the viewer).
SELECT u.id, u.email, u.created_at, tm.joined_at
FROM team_memberships tm
JOIN users u ON u.id = tm.user_id
WHERE tm.team_id = $1
  AND EXISTS (
      SELECT 1 FROM team_memberships caller
      WHERE caller.team_id = $1 AND caller.user_id = $2
  )
ORDER BY tm.joined_at ASC;

-- name: IsMemberOfTeam :one
-- Cheap predicate for the invite-confirm page ("you are already a member").
SELECT EXISTS (
    SELECT 1 FROM team_memberships
    WHERE team_id = $1 AND user_id = $2
);
