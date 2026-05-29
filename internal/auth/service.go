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

// SignupResult is what Signup returns on success.
type SignupResult struct {
	User db.User
	Team db.Team
}

// Signup creates a user, an auto-named "personal" team, and the membership
// linking the two in a single transaction. Either everything lands or the
// row state is unchanged.
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
