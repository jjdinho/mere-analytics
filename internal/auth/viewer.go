package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jjdinho/mere-analytics/internal/idgen"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
)

// Viewer is the per-request capability bag used by handlers to read and
// mutate the resources the current user is allowed to touch. Every query
// goes through a membership-gated SQL statement (JOIN team_memberships) so a
// caller can never reach data outside the teams they belong to.
//
// Construction:
//
//   request →  authMiddleware loads session  →  WithViewer(ctx, NewViewer(...))
//                                              │
//                                              ▼
//                       handler:  v := auth.ViewerFrom(r.Context())
//                                 p, err := v.Projects(ctx).ByID(projectID)
//                                 if errors.Is(err, auth.ErrNotVisible) { 404 }
//
// On membership miss every query returns ErrNotVisible — a single sentinel
// that handlers translate to 404 without distinguishing "doesn't exist" from
// "exists but not yours" (Issue 6; defends against UUID enumeration).
type Viewer struct {
	queries *db.Queries
	userID  string
}

// NewViewer builds a viewer for a specific user against a Queries handle.
// The middleware in package web constructs one per request; tests may
// construct directly against a pool's queries.
func NewViewer(queries *db.Queries, userID string) *Viewer {
	return &Viewer{queries: queries, userID: userID}
}

// UserID returns the viewer's user id. Tests and handlers occasionally need
// the raw id (e.g. for self-membership banners) without going through a
// chain.
func (v *Viewer) UserID() string { return v.userID }

// ErrNotVisible is returned by every Viewer read/write when the row either
// doesn't exist or exists but the viewer has no team membership granting
// access. Handlers map it to 404. Plan Issue 6.
var ErrNotVisible = errors.New("not visible to viewer")

type viewerContextKey struct{}

// WithViewer attaches v to ctx so downstream handlers can recover it without
// re-threading svc.Queries + userID.
func WithViewer(ctx context.Context, v *Viewer) context.Context {
	return context.WithValue(ctx, viewerContextKey{}, v)
}

// ViewerFrom returns the viewer attached to ctx, or nil for anonymous
// requests. Handlers behind requireSession can rely on a non-nil viewer.
func ViewerFrom(ctx context.Context) *Viewer {
	v, _ := ctx.Value(viewerContextKey{}).(*Viewer)
	return v
}

// ──────────────────────────────────────────────────────────────────────
// Teams
// ──────────────────────────────────────────────────────────────────────

type TeamsChain struct {
	v   *Viewer
	ctx context.Context
}

func (v *Viewer) Teams(ctx context.Context) *TeamsChain {
	return &TeamsChain{v: v, ctx: ctx}
}

// ByID returns the team if the viewer is a member; ErrNotVisible otherwise.
func (c *TeamsChain) ByID(teamID string) (db.Team, error) {
	team, err := c.v.queries.GetTeamForUser(c.ctx, db.GetTeamForUserParams{
		ID:     teamID,
		UserID: c.v.userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Team{}, ErrNotVisible
	}
	if err != nil {
		return db.Team{}, fmt.Errorf("viewer teams by id: %w", err)
	}
	return team, nil
}

// List returns every team the viewer belongs to, oldest first (signup
// auto-creates the personal team, so it's always index 0).
func (c *TeamsChain) List() ([]db.Team, error) {
	teams, err := c.v.queries.ListTeamsForUser(c.ctx, c.v.userID)
	if err != nil {
		return nil, fmt.Errorf("viewer teams list: %w", err)
	}
	return teams, nil
}

// MembersOf returns the team's members if the viewer is themselves a
// member; ErrNotVisible otherwise. Used by the team-settings page.
func (c *TeamsChain) MembersOf(teamID string) ([]db.ListMembersForTeamForUserRow, error) {
	rows, err := c.v.queries.ListMembersForTeamForUser(c.ctx, db.ListMembersForTeamForUserParams{
		TeamID: teamID,
		UserID: c.v.userID,
	})
	if err != nil {
		return nil, fmt.Errorf("viewer members of: %w", err)
	}
	if len(rows) == 0 {
		// Either the team doesn't exist or viewer isn't in it. Both → 404.
		return nil, ErrNotVisible
	}
	return rows, nil
}

// ──────────────────────────────────────────────────────────────────────
// Projects
// ──────────────────────────────────────────────────────────────────────

type ProjectsChain struct {
	v   *Viewer
	ctx context.Context
}

func (v *Viewer) Projects(ctx context.Context) *ProjectsChain {
	return &ProjectsChain{v: v, ctx: ctx}
}

// ByID returns the project if the viewer's team owns it and it's not
// soft-deleted; ErrNotVisible otherwise.
func (c *ProjectsChain) ByID(projectID string) (db.Project, error) {
	p, err := c.v.queries.GetProjectForUser(c.ctx, db.GetProjectForUserParams{
		ID:     projectID,
		UserID: c.v.userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Project{}, ErrNotVisible
	}
	if err != nil {
		return db.Project{}, fmt.Errorf("viewer projects by id: %w", err)
	}
	return p, nil
}

// ListForTeam returns active projects under the team. Empty slice for a team
// with no projects; ErrNotVisible only if the viewer can't see the team
// itself (we pre-check via the JOIN by returning zero rows on no membership
// — distinguished from the empty-team case via the team-existence sanity
// caller-side).
func (c *ProjectsChain) ListForTeam(teamID string) ([]db.Project, error) {
	rows, err := c.v.queries.ListProjectsForTeamForUser(c.ctx, db.ListProjectsForTeamForUserParams{
		TeamID: teamID,
		UserID: c.v.userID,
	})
	if err != nil {
		return nil, fmt.Errorf("viewer projects list for team: %w", err)
	}
	return rows, nil
}

// ListForTeams powers the rebuilt home page. Bounded 2-query pattern
// (Issue 15): call Teams.List then this with the resulting ids. Returns a
// flat slice grouped by team_id in iteration order.
func (c *ProjectsChain) ListForTeams(teamIDs []string) ([]db.Project, error) {
	if len(teamIDs) == 0 {
		return []db.Project{}, nil
	}
	rows, err := c.v.queries.ListProjectsForTeamsForUser(c.ctx, db.ListProjectsForTeamsForUserParams{
		Column1: teamIDs,
		UserID:  c.v.userID,
	})
	if err != nil {
		return nil, fmt.Errorf("viewer projects list for teams: %w", err)
	}
	return rows, nil
}

// Create issues a project under teamID. Caller must be a team member; if
// not, RowsAffected == 0 surfaces as ErrNotVisible.
func (c *ProjectsChain) Create(teamID, name string) (db.Project, error) {
	p, err := c.v.queries.CreateProjectForUser(c.ctx, db.CreateProjectForUserParams{
		ID:     idgen.New(),
		TeamID: teamID,
		Name:   name,
		UserID: c.v.userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// INSERT ... SELECT ... WHERE EXISTS returns no row when the EXISTS
		// guard fails (i.e., caller is not a team member).
		return db.Project{}, ErrNotVisible
	}
	if err != nil {
		return db.Project{}, fmt.Errorf("viewer projects create: %w", err)
	}
	return p, nil
}

// SoftDelete sets deleted_at on a viewer-owned project. Returns ErrNotVisible
// if the project is not in any team the viewer belongs to OR is already
// soft-deleted (collapsed for the same UUID-enumeration defense as ByID).
func (c *ProjectsChain) SoftDelete(projectID string) error {
	rows, err := c.v.queries.SoftDeleteProjectForUser(c.ctx, db.SoftDeleteProjectForUserParams{
		ID:     projectID,
		UserID: c.v.userID,
	})
	if err != nil {
		return fmt.Errorf("viewer projects soft delete: %w", err)
	}
	if rows == 0 {
		return ErrNotVisible
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────
// Tokens
// ──────────────────────────────────────────────────────────────────────

type TokensChain struct {
	v   *Viewer
	ctx context.Context
}

func (v *Viewer) Tokens(ctx context.Context) *TokensChain {
	return &TokensChain{v: v, ctx: ctx}
}

// ListForProject returns active tokens for the project. Empty slice for "no
// tokens"; ErrNotVisible is NOT returned here because handlers always check
// project visibility via Projects.ByID first.
func (c *TokensChain) ListForProject(projectID string) ([]db.ApiToken, error) {
	rows, err := c.v.queries.ListTokensForProjectForUser(c.ctx, db.ListTokensForProjectForUserParams{
		ProjectID: projectID,
		UserID:    c.v.userID,
	})
	if err != nil {
		return nil, fmt.Errorf("viewer tokens list: %w", err)
	}
	return rows, nil
}

// CreateTokenResult is what Create returns: the plaintext token (display
// once via render-on-POST, then discard) and the persisted row (hash only).
type CreateTokenResult struct {
	Plaintext string
	Token     db.ApiToken
}

// Create issues a token under projectID. Plaintext is returned by value
// once; the caller must render it immediately and not re-issue it.
func (c *TokensChain) Create(projectID, name string) (*CreateTokenResult, error) {
	plaintext, hashHex, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	row, err := c.v.queries.CreateAPITokenForUser(c.ctx, db.CreateAPITokenForUserParams{
		ID:        idgen.New(),
		ProjectID: projectID,
		Name:      name,
		TokenHash: hashHex,
		UserID:    c.v.userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotVisible
	}
	if err != nil {
		return nil, fmt.Errorf("viewer tokens create: %w", err)
	}
	return &CreateTokenResult{Plaintext: plaintext, Token: row}, nil
}

// Revoke sets revoked_at on a viewer-owned token. Already-revoked tokens
// return ErrNotVisible (collapsed with "not yours" / "wrong project" for the
// same enumeration defense).
func (c *TokensChain) Revoke(projectID, tokenID string) error {
	rows, err := c.v.queries.RevokeAPITokenForUser(c.ctx, db.RevokeAPITokenForUserParams{
		ID:        tokenID,
		ProjectID: projectID,
		UserID:    c.v.userID,
	})
	if err != nil {
		return fmt.Errorf("viewer tokens revoke: %w", err)
	}
	if rows == 0 {
		return ErrNotVisible
	}
	return nil
}

// ──────────────────────────────────────────────────────────────────────
// Team invites (issuance side; consume lives on Service due to tx need)
// ──────────────────────────────────────────────────────────────────────

// InviteTTL is how long a generated invite stays valid before its
// expires_at trips. Plan §"Decisions for this step" — 7-day TTL.
const InviteTTL = 7 * 24 * time.Hour

// InviteResult is what CreateInvite returns: the plaintext token (embed in
// the URL the inviter shares) and the persisted row.
type InviteResult struct {
	Plaintext string
	Invite    db.TeamInvite
}

// CreateInvite issues a one-shot invite for teamID; caller must be a team
// member. Returns ErrNotVisible on missing membership.
func (v *Viewer) CreateInvite(ctx context.Context, teamID string, now time.Time) (*InviteResult, error) {
	plaintext, hashHex, err := GenerateToken()
	if err != nil {
		return nil, err
	}
	row, err := v.queries.CreateTeamInviteForUser(ctx, db.CreateTeamInviteForUserParams{
		ID:        idgen.New(),
		TeamID:    teamID,
		CreatedBy: v.userID,
		TokenHash: hashHex,
		ExpiresAt: pgtype.Timestamptz{Time: now.Add(InviteTTL), Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotVisible
	}
	if err != nil {
		return nil, fmt.Errorf("viewer invites create: %w", err)
	}
	return &InviteResult{Plaintext: plaintext, Invite: row}, nil
}
