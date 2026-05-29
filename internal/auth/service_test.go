package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jjdinho/mere-analytics/internal/auth"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

func runMigrations(t *testing.T) (*auth.Service, *db.Queries) {
	t.Helper()
	ctx := context.Background()
	pool, cfg := testhelpers.StartPostgres(t)

	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := mmigrate.Run(ctx, "pg", drv, migrations.Postgres, "postgres", logger); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return auth.NewService(pool), db.New(pool)
}

func TestSignup_atomicityAcrossUserTeamMembership(t *testing.T) {
	svc, queries := runMigrations(t)
	ctx := context.Background()

	res, err := svc.Signup(ctx, auth.SignupRequest{
		Email:    "Owner@Example.com",
		Password: "correct horse battery staple",
	})
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}
	if res.User.Email != "owner@example.com" {
		t.Errorf("user email not normalized: %q", res.User.Email)
	}

	user, err := queries.GetUserByID(ctx, res.User.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if user.Email != "owner@example.com" {
		t.Errorf("user.Email: %q", user.Email)
	}
	if !auth.VerifyPassword(user.PasswordHash, "correct horse battery staple") {
		t.Errorf("stored hash does not verify against original password")
	}

	teams, err := queries.ListTeamsForUser(ctx, res.User.ID)
	if err != nil {
		t.Fatalf("ListTeamsForUser: %v", err)
	}
	if len(teams) != 1 {
		t.Fatalf("expected 1 team, got %d", len(teams))
	}
	if teams[0].ID != res.Team.ID {
		t.Errorf("team mismatch: %q != %q", teams[0].ID, res.Team.ID)
	}
}

func TestSignup_duplicateEmailReturnsErrEmailTaken(t *testing.T) {
	svc, _ := runMigrations(t)
	ctx := context.Background()

	if _, err := svc.Signup(ctx, auth.SignupRequest{
		Email: "dup@example.com", Password: "correct horse battery staple",
	}); err != nil {
		t.Fatalf("first Signup: %v", err)
	}
	// Same email, different case → still rejected.
	_, err := svc.Signup(ctx, auth.SignupRequest{
		Email: "DUP@example.com", Password: "correct horse battery staple",
	})
	if !errors.Is(err, auth.ErrEmailTaken) {
		t.Fatalf("duplicate signup: got %v want ErrEmailTaken", err)
	}
}

func TestSignup_rejectsShortPassword(t *testing.T) {
	svc, _ := runMigrations(t)
	_, err := svc.Signup(context.Background(), auth.SignupRequest{
		Email: "user@example.com", Password: "short",
	})
	var ve *auth.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected ValidationError, got %v", err)
	}
}

func TestAuthenticate_okAndInvalid(t *testing.T) {
	svc, _ := runMigrations(t)
	ctx := context.Background()

	if _, err := svc.Signup(ctx, auth.SignupRequest{
		Email: "user@example.com", Password: "correct horse battery staple",
	}); err != nil {
		t.Fatalf("Signup: %v", err)
	}

	u, err := svc.Authenticate(ctx, "USER@example.com", "correct horse battery staple")
	if err != nil {
		t.Fatalf("Authenticate(correct): %v", err)
	}
	if u.Email != "user@example.com" {
		t.Errorf("returned user email: %q", u.Email)
	}

	if _, err := svc.Authenticate(ctx, "user@example.com", "wrong"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Authenticate(wrong pw): %v", err)
	}
	if _, err := svc.Authenticate(ctx, "ghost@example.com", "anything goes here"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("Authenticate(unknown email): %v", err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	svc, _ := runMigrations(t)
	ctx := context.Background()

	res, err := svc.Signup(ctx, auth.SignupRequest{
		Email: "user@example.com", Password: "correct horse battery staple",
	})
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}

	sess, err := svc.CreateSession(ctx, res.User.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.CSRFToken == "" {
		t.Errorf("session has empty CSRF token")
	}

	got, err := svc.LookupSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("LookupSession: %v", err)
	}
	if got.UserID != res.User.ID {
		t.Errorf("session user mismatch")
	}

	// Destroy → subsequent lookup is ErrSessionNotFound.
	if err := svc.DestroySession(ctx, sess.ID); err != nil {
		t.Fatalf("DestroySession: %v", err)
	}
	if _, err := svc.LookupSession(ctx, sess.ID); !errors.Is(err, auth.ErrSessionNotFound) {
		t.Errorf("LookupSession(destroyed): %v", err)
	}
}

func TestLookupSession_expired(t *testing.T) {
	svc, _ := runMigrations(t)
	ctx := context.Background()

	res, err := svc.Signup(ctx, auth.SignupRequest{
		Email: "user@example.com", Password: "correct horse battery staple",
	})
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}

	// Drive the clock manually: create a session at t0, advance now() past
	// its sliding window, expect Lookup to return ErrSessionExpired.
	t0 := time.Now().UTC()
	svc.SetNow(func() time.Time { return t0 })
	sess, err := svc.CreateSession(ctx, res.User.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	advanced := sess.ExpiresAt.Add(time.Second)
	svc.SetNow(func() time.Time { return advanced })

	if _, err := svc.LookupSession(ctx, sess.ID); !errors.Is(err, auth.ErrSessionExpired) {
		t.Errorf("LookupSession(expired): got %v want ErrSessionExpired", err)
	}
}

func TestTouchSession_extendsWithinCap(t *testing.T) {
	svc, _ := runMigrations(t)
	ctx := context.Background()

	res, err := svc.Signup(ctx, auth.SignupRequest{
		Email: "user@example.com", Password: "correct horse battery staple",
	})
	if err != nil {
		t.Fatalf("Signup: %v", err)
	}

	t0 := time.Now().UTC()
	svc.SetNow(func() time.Time { return t0 })
	sess, err := svc.CreateSession(ctx, res.User.ID)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	originalExpiry := sess.ExpiresAt

	// Advance 1 day; touch should bump expiry forward (still inside max).
	svc.SetNow(func() time.Time { return t0.Add(24 * time.Hour) })
	newExpiry, err := svc.TouchSession(ctx, sess)
	if err != nil {
		t.Fatalf("TouchSession: %v", err)
	}
	if !newExpiry.After(originalExpiry) {
		t.Errorf("touch should advance expiry: was %v now %v", originalExpiry, newExpiry)
	}
}
