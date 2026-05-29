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

-- name: ListTeamsForUser :many
SELECT t.id, t.name, t.created_at
FROM teams t
JOIN team_memberships tm ON tm.team_id = t.id
WHERE tm.user_id = $1
ORDER BY t.created_at ASC;
