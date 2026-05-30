// Package auth owns the application's authentication primitives: bcrypt
// password hashing, CSRF token generation, and the session lifecycle
// (create / lookup / touch / destroy). The Service type bundles a pgx pool
// with the sqlc-generated db.Queries and exposes Signup + Authenticate so
// callers don't reach into the DB directly.
//
// Session expiry policy:
//   - sliding window of 7 days bumped on every authenticated request via Touch
//   - hard cap of 30 days from CreatedAt; Touch will never extend past it
//
// Two configurable knobs (SlidingWindow / MaxLifetime) exist so tests can run
// against shrunk windows. Production callers leave them at the defaults.
package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jjdinho/mere-analytics/internal/idgen"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
)

// DefaultSlidingWindow is how far into the future Touch pushes ExpiresAt on
// each authenticated request.
const DefaultSlidingWindow = 7 * 24 * time.Hour

// DefaultMaxLifetime caps how long a session can live from CreatedAt no matter
// how often it's touched.
const DefaultMaxLifetime = 30 * 24 * time.Hour

// SessionCookieName is the cookie name used by web.SessionMiddleware.
const SessionCookieName = "mere_session"

// ErrEmailTaken is returned by Signup when a user with the same email
// (case-insensitive) already exists.
var ErrEmailTaken = errors.New("email already registered")

// ErrInvalidCredentials is returned by Authenticate when either the email is
// unknown or the password is wrong. The two cases are deliberately conflated
// to avoid leaking which accounts exist.
var ErrInvalidCredentials = errors.New("invalid credentials")

// ErrSessionNotFound is returned by LookupSession when the cookie's session id
// doesn't exist in the sessions table.
var ErrSessionNotFound = errors.New("session not found")

// ErrSessionExpired is returned by LookupSession when the row exists but
// expires_at is in the past.
var ErrSessionExpired = errors.New("session expired")

// ErrInviteInvalid is returned by ConsumeInvite / SignupWithInvite when the
// invite token doesn't resolve to an active, unexpired row. The four miss
// cases — unknown, consumed, expired, malformed — are collapsed for the
// same enumeration defense as ErrNotVisible.
var ErrInviteInvalid = errors.New("invite is no longer valid")

// ErrCurrentPasswordWrong is returned by ChangePassword when the caller
// supplied the wrong current password. Distinguished from ErrInvalidCredentials
// because the user is already authenticated; the form renders a specific
// "current password is incorrect" message.
var ErrCurrentPasswordWrong = errors.New("current password is incorrect")

// Service bundles the dependencies needed for password / session operations.
type Service struct {
	pool          *pgxpool.Pool
	queries       *db.Queries
	now           func() time.Time
	SlidingWindow time.Duration
	MaxLifetime   time.Duration
}

// NewService returns a Service backed by pool. The defaults can be overridden
// after construction (tests do this; production code leaves them alone).
func NewService(pool *pgxpool.Pool) *Service {
	return &Service{
		pool:          pool,
		queries:       db.New(pool),
		now:           time.Now,
		SlidingWindow: DefaultSlidingWindow,
		MaxLifetime:   DefaultMaxLifetime,
	}
}

// SignupRequest carries the validated form fields from the signup handler.
type SignupRequest struct {
	Email    string
	Password string
}

// SignupResult is what Signup / SignupWithInvite return on success.
//
// Team is always the user's auto-created personal team. InvitedTeam is the
// team named by the invite token and is only populated when the call went
// through SignupWithInvite — Signup leaves it as the zero db.Team. Web
// callers redirect to InvitedTeam after invite-based signup so the user
// lands where they expected.
type SignupResult struct {
	User        db.User
	Team        db.Team
	InvitedTeam db.Team
}

// Signup creates a user, an auto-named "personal" team, and the membership
// linking the two in a single transaction. Either everything lands or the
// row state is unchanged.
//
// NOT wired to any HTTP route. The public /signup endpoint was removed —
// production user creation goes through SignupWithInvite (invite-based
// web flow) or scripts/operator/create-user.sql (operator bootstrap). This
// function is retained as a seed primitive for tests; do not reintroduce
// an HTTP handler that calls it.
func (s *Service) Signup(ctx context.Context, req SignupRequest) (*SignupResult, error) {
	email := NormalizeEmail(req.Email)
	if err := ValidateEmail(email); err != nil {
		return nil, err
	}
	if err := ValidatePassword(req.Password); err != nil {
		return nil, err
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.queries.WithTx(tx)

	user, err := q.CreateUser(ctx, db.CreateUserParams{
		ID:                 idgen.New(),
		Email:              email,
		PasswordHash:       hash,
		MustChangePassword: false,
	})
	if err != nil {
		if isUniqueViolation(err, "users_email_lower_idx") {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("create user: %w", err)
	}

	team, err := q.CreateTeam(ctx, db.CreateTeamParams{
		ID:   idgen.New(),
		Name: defaultTeamName(email),
	})
	if err != nil {
		return nil, fmt.Errorf("create team: %w", err)
	}

	if err := q.CreateTeamMembership(ctx, db.CreateTeamMembershipParams{
		TeamID: team.ID,
		UserID: user.ID,
	}); err != nil {
		return nil, fmt.Errorf("create membership: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}

	return &SignupResult{User: user, Team: team}, nil
}

// Authenticate verifies an email + password pair and returns the corresponding
// user row on success. The error path collapses unknown-email and wrong-password
// into ErrInvalidCredentials.
func (s *Service) Authenticate(ctx context.Context, rawEmail, password string) (db.User, error) {
	email := NormalizeEmail(rawEmail)
	user, err := s.queries.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.User{}, ErrInvalidCredentials
		}
		return db.User{}, fmt.Errorf("lookup user: %w", err)
	}
	if !VerifyPassword(user.PasswordHash, password) {
		return db.User{}, ErrInvalidCredentials
	}
	return user, nil
}

// CreateSession issues a new session row for userID with a fresh CSRF token,
// returning the populated session ready to be set as a cookie.
func (s *Service) CreateSession(ctx context.Context, userID string) (*Session, error) {
	csrf, err := GenerateCSRFToken()
	if err != nil {
		return nil, err
	}
	now := s.now()
	expires := now.Add(s.SlidingWindow)
	if cap := now.Add(s.MaxLifetime); expires.After(cap) {
		expires = cap
	}

	row, err := s.queries.CreateSession(ctx, db.CreateSessionParams{
		ID:        idgen.New(),
		UserID:    userID,
		ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true},
		CsrfToken: csrf,
	})
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	user, err := s.queries.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("fetch user for session: %w", err)
	}
	return &Session{
		ID:                 row.ID,
		UserID:             row.UserID,
		UserEmail:          user.Email,
		CSRFToken:          row.CsrfToken,
		CreatedAt:          row.CreatedAt.Time,
		ExpiresAt:          row.ExpiresAt.Time,
		MustChangePassword: user.MustChangePassword,
	}, nil
}

// LookupSession resolves a session id to its joined session+user row.
// Returns ErrSessionNotFound for an unknown id and ErrSessionExpired when
// the row's ExpiresAt is in the past; the latter is also opportunistically
// deleted (best-effort; ignored on failure).
func (s *Service) LookupSession(ctx context.Context, id string) (*Session, error) {
	row, err := s.queries.GetSessionWithUser(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("lookup session: %w", err)
	}
	now := s.now()
	if !row.ExpiresAt.Valid || !row.ExpiresAt.Time.After(now) {
		_ = s.queries.DeleteSession(ctx, id)
		return nil, ErrSessionExpired
	}
	return &Session{
		ID:                 row.SessionID,
		UserID:             row.UserID,
		UserEmail:          row.UserEmail,
		CSRFToken:          row.CsrfToken,
		CreatedAt:          row.CreatedAt.Time,
		ExpiresAt:          row.ExpiresAt.Time,
		MustChangePassword: row.MustChangePassword,
	}, nil
}

// TouchSession pushes ExpiresAt forward by SlidingWindow, capped at
// CreatedAt + MaxLifetime. Returns the new ExpiresAt the caller should use
// for the cookie's Max-Age.
func (s *Service) TouchSession(ctx context.Context, sess *Session) (time.Time, error) {
	now := s.now()
	target := now.Add(s.SlidingWindow)
	if cap := sess.CreatedAt.Add(s.MaxLifetime); target.After(cap) {
		target = cap
	}
	if !target.After(sess.ExpiresAt) {
		// Already at or past target — skip the write.
		return sess.ExpiresAt, nil
	}
	if err := s.queries.TouchSession(ctx, db.TouchSessionParams{
		ID:        sess.ID,
		ExpiresAt: pgtype.Timestamptz{Time: target, Valid: true},
	}); err != nil {
		return time.Time{}, fmt.Errorf("touch session: %w", err)
	}
	sess.ExpiresAt = target
	return target, nil
}

// DestroySession deletes the row identified by id. Missing rows are not an
// error — logout against an already-expired session should still succeed.
func (s *Service) DestroySession(ctx context.Context, id string) error {
	if err := s.queries.DeleteSession(ctx, id); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// computeExpiry is the math behind Create/Touch, exported only for tests.
func (s *Service) computeExpiry(createdAt time.Time) time.Time {
	now := s.now()
	target := now.Add(s.SlidingWindow)
	if cap := createdAt.Add(s.MaxLifetime); target.After(cap) {
		target = cap
	}
	return target
}

// SetNow swaps the Service's clock. Test-only.
func (s *Service) SetNow(fn func() time.Time) { s.now = fn }

func defaultTeamName(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return "Personal"
	}
	return email[:at] + "'s team"
}

// isUniqueViolation reports whether err is a Postgres unique_violation against
// the named constraint. Used to translate raw 23505s into ErrEmailTaken without
// false positives from other unique indexes.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23505" {
		return false
	}
	return pgErr.ConstraintName == constraint
}

// Queries exposes the service's sqlc handle. The web middleware uses this to
// build per-request viewers (auth.Viewer) without re-wiring the pool.
func (s *Service) Queries() *db.Queries { return s.queries }

// ──────────────────────────────────────────────────────────────────────
// Project create (with auto-provisioned public token)
// ──────────────────────────────────────────────────────────────────────

// createProjectWithPublicToken inserts the project + its bootstrap public
// ingest token in a single transaction. Membership is enforced by the
// project INSERT's WHERE EXISTS clause; pgx.ErrNoRows on that step (no
// matching membership row) surfaces as ErrNotVisible and the tx aborts.
//
// The public token is the snippet token that lives in client HTML — it's
// non-secret. We persist both token_plaintext (so the project page can
// re-display it on every visit) and token_hash (for the future bearer
// middleware's indexed lookup). The partial unique index
// api_tokens_one_active_public_per_project_idx guarantees at most one
// active public token per project; a concurrent insert would fail loudly
// and abort the tx.
//
// Called via Viewer.Projects.Create. Lowercase because external callers
// should go through the viewer chain (matches ConsumeInvite's exported
// pattern — exported only when the web layer calls it directly).
func (s *Service) createProjectWithPublicToken(ctx context.Context, userID, teamID, name string) (db.Project, error) {
	plaintext, hashHex, err := GenerateToken(TokenKindPublic)
	if err != nil {
		return db.Project{}, fmt.Errorf("project create: generate public token: %w", err)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return db.Project{}, fmt.Errorf("project create: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	proj, err := q.CreateProjectForUser(ctx, db.CreateProjectForUserParams{
		ID:     idgen.New(),
		TeamID: teamID,
		Name:   name,
		UserID: userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// INSERT ... SELECT ... WHERE EXISTS returned no row — caller is not
		// a team member.
		return db.Project{}, ErrNotVisible
	}
	if err != nil {
		return db.Project{}, fmt.Errorf("project create: insert project: %w", err)
	}

	if err := q.InsertPublicAPIToken(ctx, db.InsertPublicAPITokenParams{
		ID:             idgen.New(),
		ProjectID:      proj.ID,
		Name:           "snippet",
		TokenHash:      hashHex,
		TokenPlaintext: &plaintext,
	}); err != nil {
		return db.Project{}, fmt.Errorf("project create: insert public token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return db.Project{}, fmt.Errorf("project create: commit: %w", err)
	}
	return proj, nil
}

// ──────────────────────────────────────────────────────────────────────
// Invite consume
// ──────────────────────────────────────────────────────────────────────

// ConsumeInvite atomically burns the invite token and adds userID to the
// team, in a single transaction:
//
//   BEGIN
//     UPDATE team_invites SET consumed_at=NOW(), consumed_by=$user
//       WHERE token_hash=$h AND consumed_at IS NULL AND expires_at > NOW()
//       RETURNING team_id                       ──┐
//     INSERT INTO team_memberships ...           │  same tx
//   COMMIT                                       └──┘
//
// Outcomes (Issues 7, 10):
//   - invite valid + user not yet a member  → membership created, returns team
//   - invite valid + user already a member  → silent success, invite burned
//   - invite consumed/expired/unknown       → ErrInviteInvalid
func (s *Service) ConsumeInvite(ctx context.Context, userID, plaintextToken string) (db.Team, error) {
	hashHex := HashToken(plaintextToken)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return db.Team{}, fmt.Errorf("invite tx begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	invite, err := q.ConsumeInviteByHash(ctx, db.ConsumeInviteByHashParams{
		TokenHash:  hashHex,
		ConsumedBy: &userID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return db.Team{}, ErrInviteInvalid
	}
	if err != nil {
		return db.Team{}, fmt.Errorf("invite consume: %w", err)
	}

	// Insert membership via ON CONFLICT DO NOTHING — tolerates "already a
	// member" without aborting the transaction (Issue 10 silent success).
	if err := q.CreateTeamMembershipIfMissing(ctx, db.CreateTeamMembershipIfMissingParams{
		TeamID: invite.TeamID,
		UserID: userID,
	}); err != nil {
		return db.Team{}, fmt.Errorf("invite membership insert: %w", err)
	}

	team, err := q.GetTeamByID(ctx, invite.TeamID)
	if err != nil {
		return db.Team{}, fmt.Errorf("invite team fetch: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return db.Team{}, fmt.Errorf("invite tx commit: %w", err)
	}
	return team, nil
}

// ──────────────────────────────────────────────────────────────────────
// Signup with invite (Issue 12 — strict)
// ──────────────────────────────────────────────────────────────────────

// SignupWithInvite runs the standard signup tx and consumes the invite
// token in the same tx. If the invite is invalid at POST time, the entire
// signup is aborted with ErrInviteInvalid — Issue 12's strict path; the
// caller (the anon branch of /invites/{token} POST) re-renders the page
// as InviteInvalid.
//
// invitePlaintext is required — public open-signup is no longer supported.
// Operator-bootstrapped users go through scripts/operator/create-user.sql,
// which bypasses this path entirely.
//
// On success, SignupResult.Team is the user's personal team and
// SignupResult.InvitedTeam is the team the invite belongs to.
func (s *Service) SignupWithInvite(ctx context.Context, req SignupRequest, invitePlaintext string) (*SignupResult, error) {
	if invitePlaintext == "" {
		return nil, ErrInviteInvalid
	}

	email := NormalizeEmail(req.Email)
	if err := ValidateEmail(email); err != nil {
		return nil, err
	}
	if err := ValidatePassword(req.Password); err != nil {
		return nil, err
	}
	hash, err := HashPassword(req.Password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.queries.WithTx(tx)

	user, err := q.CreateUser(ctx, db.CreateUserParams{
		ID:                 idgen.New(),
		Email:              email,
		PasswordHash:       hash,
		MustChangePassword: false,
	})
	if err != nil {
		if isUniqueViolation(err, "users_email_lower_idx") {
			return nil, ErrEmailTaken
		}
		return nil, fmt.Errorf("create user: %w", err)
	}

	personal, err := q.CreateTeam(ctx, db.CreateTeamParams{
		ID:   idgen.New(),
		Name: defaultTeamName(email),
	})
	if err != nil {
		return nil, fmt.Errorf("create team: %w", err)
	}
	if err := q.CreateTeamMembership(ctx, db.CreateTeamMembershipParams{
		TeamID: personal.ID,
		UserID: user.ID,
	}); err != nil {
		return nil, fmt.Errorf("create membership: %w", err)
	}

	// Consume the invite inside the same tx. Failure aborts the whole signup
	// so the user is bounced back to the form (Issue 12 strict).
	invitedHash := HashToken(invitePlaintext)
	consumedID := user.ID
	invite, err := q.ConsumeInviteByHash(ctx, db.ConsumeInviteByHashParams{
		TokenHash:  invitedHash,
		ConsumedBy: &consumedID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInviteInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("signup invite consume: %w", err)
	}
	// ON CONFLICT DO NOTHING for consistency with ConsumeInvite — "already a
	// member" can't happen on a fresh signup but matches the same rule.
	if err := q.CreateTeamMembershipIfMissing(ctx, db.CreateTeamMembershipIfMissingParams{
		TeamID: invite.TeamID,
		UserID: user.ID,
	}); err != nil {
		return nil, fmt.Errorf("signup invite membership: %w", err)
	}

	invitedTeam, err := q.GetTeamByID(ctx, invite.TeamID)
	if err != nil {
		return nil, fmt.Errorf("signup invite team fetch: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return &SignupResult{User: user, Team: personal, InvitedTeam: invitedTeam}, nil
}

// ──────────────────────────────────────────────────────────────────────
// Change password
// ──────────────────────────────────────────────────────────────────────

// ChangePassword swaps a user's password after verifying the current one,
// and clears must_change_password on success. Length policy is enforced via
// ValidatePassword (returns *ValidationError so the form can surface it).
//
// The current-password check is the same as Authenticate without the
// email-collapse — we already know the userID from the session.
func (s *Service) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	user, err := s.queries.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("change password: lookup user: %w", err)
	}
	if !VerifyPassword(user.PasswordHash, currentPassword) {
		return ErrCurrentPasswordWrong
	}
	newHash, err := HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("change password: hash: %w", err)
	}
	if err := s.queries.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{
		ID:                 userID,
		PasswordHash:       newHash,
		MustChangePassword: false,
	}); err != nil {
		return fmt.Errorf("change password: update: %w", err)
	}
	return nil
}
