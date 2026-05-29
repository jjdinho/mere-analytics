-- All project queries are membership-gated via a JOIN on team_memberships.
-- A row that doesn't belong to a team the viewer is in returns zero rows,
-- which the viewer translates to auth.ErrNotVisible (Issue 6).
-- Soft-deleted rows (deleted_at IS NOT NULL) are excluded from every read.

-- name: GetProjectForUser :one
SELECT p.id, p.team_id, p.name, p.created_at, p.deleted_at
FROM projects p
JOIN team_memberships tm ON tm.team_id = p.team_id
WHERE p.id = $1
  AND tm.user_id = $2
  AND p.deleted_at IS NULL;

-- name: ListProjectsForTeamForUser :many
SELECT p.id, p.team_id, p.name, p.created_at, p.deleted_at
FROM projects p
JOIN team_memberships tm ON tm.team_id = p.team_id
WHERE p.team_id = $1
  AND tm.user_id = $2
  AND p.deleted_at IS NULL
ORDER BY p.created_at ASC;

-- name: ListProjectsForTeamsForUser :many
-- Used by the rebuilt home page (Issue 15): bounded 2-query pattern.
SELECT p.id, p.team_id, p.name, p.created_at, p.deleted_at
FROM projects p
JOIN team_memberships tm ON tm.team_id = p.team_id
WHERE p.team_id = ANY($1::uuid[])
  AND tm.user_id = $2
  AND p.deleted_at IS NULL
ORDER BY p.team_id ASC, p.created_at ASC;

-- name: CreateProjectForUser :one
-- The WHERE EXISTS guard makes the INSERT a no-op when the caller is not a
-- member of the target team. RowsAffected == 0 → auth.ErrNotVisible.
INSERT INTO projects (id, team_id, name)
SELECT $1, $2, $3
WHERE EXISTS (
    SELECT 1 FROM team_memberships
    WHERE team_id = $2 AND user_id = $4
)
RETURNING id, team_id, name, created_at, deleted_at;

-- name: SoftDeleteProjectForUser :execrows
-- Returns RowsAffected; 0 means either not-a-member or already-deleted, both
-- of which collapse to ErrNotVisible at the viewer.
UPDATE projects
SET deleted_at = NOW()
WHERE id = $1
  AND deleted_at IS NULL
  AND team_id IN (
      SELECT team_id FROM team_memberships WHERE user_id = $2
  );
