package maintenance_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/maintenance"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func setupFixture(t *testing.T) (*pgxpool.Pool, *auth.Service, *oauth.Service) {
	t.Helper()
	pool, cfg := testhelpers.StartPostgres(t)
	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	if err := mmigrate.Run(context.Background(), "pg", drv, migrations.Postgres, "postgres", discardLogger()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool, auth.NewService(pool), oauth.NewService(pool)
}

func pkceChallenge(t *testing.T) string {
	t.Helper()
	verifier := base64.RawURLEncoding.EncodeToString([]byte("test-verifier-32-bytes-of-entropy-z"))
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// TestRun_DeletesOnlyExpired seeds one fresh + one stale row in each of the
// three tables maintenance owns, runs the sweep, and pins both the per-table
// delete counts and the surviving-row counts.
func TestRun_DeletesOnlyExpired(t *testing.T) {
	ctx := context.Background()
	pool, authSvc, oauthSvc := setupFixture(t)

	sr, err := authSvc.Signup(ctx, auth.SignupRequest{
		Email: "alice@example.com", Password: "correct horse battery staple",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	viewer := auth.NewViewer(authSvc, sr.User.ID)
	proj, err := viewer.Projects(ctx).Create(sr.Team.ID, "p")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	client, err := oauthSvc.RegisterClient(ctx, oauth.RegisterParams{
		Name: "cli", RedirectURIs: []string{"http://localhost:9999/cb"},
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	challenge := pkceChallenge(t)

	issueCode := func() {
		if _, err := oauthSvc.IssueCode(ctx, oauth.IssueCodeParams{
			ClientID: client.ID, UserID: sr.User.ID, ProjectID: proj.ID,
			RedirectURI: "http://localhost:9999/cb", Scope: oauth.ScopeAPI,
			CodeChallenge: challenge, CodeChallengeMethod: oauth.CodeChallengeMethodS256,
		}); err != nil {
			t.Fatalf("issue code: %v", err)
		}
	}
	issueAccess := func() {
		if _, _, err := oauthSvc.IssueAccessToken(ctx, oauth.IssueAccessTokenParams{
			ClientID: client.ID, UserID: sr.User.ID, ProjectID: proj.ID,
			Scope: oauth.ScopeAPI,
		}); err != nil {
			t.Fatalf("issue access token: %v", err)
		}
	}
	issueSession := func() {
		if _, err := authSvc.CreateSession(ctx, sr.User.ID); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	// Fresh row in each table — issued at real time.
	issueCode()
	issueAccess()
	issueSession()

	// Stale row in each table — backdated clocks push expires_at into the past.
	oauthSvc.SetNow(func() time.Time { return time.Now().Add(-2 * time.Hour) })
	authSvc.SetNow(func() time.Time { return time.Now().Add(-365 * 24 * time.Hour) })
	issueCode()
	issueAccess()
	issueSession()
	oauthSvc.SetNow(time.Now)
	authSvc.SetNow(time.Now)

	res, err := maintenance.Run(ctx, pool, discardLogger())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.OAuthCodes != 1 {
		t.Errorf("OAuthCodes deleted: got %d want 1", res.OAuthCodes)
	}
	if res.OAuthAccessTokens != 1 {
		t.Errorf("OAuthAccessTokens deleted: got %d want 1", res.OAuthAccessTokens)
	}
	if res.Sessions != 1 {
		t.Errorf("Sessions deleted: got %d want 1", res.Sessions)
	}

	for _, tc := range []struct {
		table string
		want  int
	}{
		{"oauth_codes", 1},
		{"oauth_access_tokens", 1},
		{"sessions", 1},
	} {
		var got int
		if err := pool.QueryRow(ctx, "SELECT count(*) FROM "+tc.table).Scan(&got); err != nil {
			t.Fatalf("count %s: %v", tc.table, err)
		}
		if got != tc.want {
			t.Errorf("%s rows remaining: got %d want %d", tc.table, got, tc.want)
		}
	}
}

// TestRun_NoExpiredRows_NoOp guards against the sweep returning bogus counts
// or erroring on an empty database.
func TestRun_NoExpiredRows_NoOp(t *testing.T) {
	ctx := context.Background()
	pool, _, _ := setupFixture(t)

	res, err := maintenance.Run(ctx, pool, discardLogger())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.OAuthCodes != 0 || res.OAuthAccessTokens != 0 || res.Sessions != 0 {
		t.Errorf("expected zero counts, got %+v", res)
	}
}

// TestRun_LeavesRevokedButUnexpiredTokens pins the deliberate decision to
// keep revoked-but-not-yet-expired access tokens around. Operational hygiene
// is the only goal; preserving the row leaves space for a future audit view.
func TestRun_LeavesRevokedButUnexpiredTokens(t *testing.T) {
	ctx := context.Background()
	pool, authSvc, oauthSvc := setupFixture(t)

	sr, err := authSvc.Signup(ctx, auth.SignupRequest{
		Email: "alice@example.com", Password: "correct horse battery staple",
	})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	viewer := auth.NewViewer(authSvc, sr.User.ID)
	proj, err := viewer.Projects(ctx).Create(sr.Team.ID, "p")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	client, err := oauthSvc.RegisterClient(ctx, oauth.RegisterParams{
		Name: "cli", RedirectURIs: []string{"http://localhost:9999/cb"},
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}

	plaintext, _, err := oauthSvc.IssueAccessToken(ctx, oauth.IssueAccessTokenParams{
		ClientID: client.ID, UserID: sr.User.ID, ProjectID: proj.ID, Scope: oauth.ScopeAPI,
	})
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE oauth_access_tokens SET revoked_at = NOW()"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_ = plaintext

	res, err := maintenance.Run(ctx, pool, discardLogger())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.OAuthAccessTokens != 0 {
		t.Errorf("revoked-but-fresh token should survive sweep; got deleted count %d", res.OAuthAccessTokens)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM oauth_access_tokens").Scan(&n); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if n != 1 {
		t.Errorf("oauth_access_tokens rows remaining: got %d want 1", n)
	}
}
