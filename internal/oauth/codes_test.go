package oauth_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jjdinho/mere-analytics/internal/auth"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

// fixture spins up Postgres + runs migrations, returns an oauth.Service +
// auth.Service + the pool so tests can seed users/projects directly.
func fixture(t *testing.T) (*oauth.Service, *auth.Service, *pgxpool.Pool) {
	t.Helper()
	pool, cfg := testhelpers.StartPostgres(t)
	drv, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := mmigrate.Run(context.Background(), "pg", drv, migrations.Postgres, "postgres", logger); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	authSvc := auth.NewService(pool)
	oauthSvc := oauth.NewService(pool)
	return oauthSvc, authSvc, pool
}

// seedUserAndProject creates a user + personal team + project and returns
// (userID, projectID).
func seedUserAndProject(t *testing.T, authSvc *auth.Service, email string) (string, string) {
	t.Helper()
	ctx := context.Background()
	res, err := authSvc.Signup(ctx, auth.SignupRequest{Email: email, Password: "correct horse battery staple"})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	v := auth.NewViewer(authSvc, res.User.ID)
	proj, err := v.Projects(ctx).Create(res.Team.ID, "p")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return res.User.ID, proj.ID
}

// registerClient seeds an OAuth client with the given redirect URI.
func registerClient(t *testing.T, svc *oauth.Service, redirect string) oauth.Client {
	t.Helper()
	c, err := svc.RegisterClient(context.Background(), oauth.RegisterParams{
		Name:         "cli",
		RedirectURIs: []string{redirect},
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	return c
}

func pkcePair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = base64.RawURLEncoding.EncodeToString([]byte("test-verifier-32-bytes-of-entropy-z"))
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func TestIssueCode_ConsumeCode_HappyPath(t *testing.T) {
	oauthSvc, authSvc, _ := fixture(t)
	userID, projectID := seedUserAndProject(t, authSvc, "alice@example.com")
	client := registerClient(t, oauthSvc, "http://localhost:9999/cb")
	verifier, challenge := pkcePair(t)
	ctx := context.Background()

	code, err := oauthSvc.IssueCode(ctx, oauth.IssueCodeParams{
		ClientID:            client.ID,
		UserID:              userID,
		ProjectID:           projectID,
		RedirectURI:         "http://localhost:9999/cb",
		Scope:               oauth.ScopeAPI,
		CodeChallenge:       challenge,
		CodeChallengeMethod: oauth.CodeChallengeMethodS256,
	})
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	if len(code) != 43 {
		t.Errorf("code length: got %d want 43", len(code))
	}

	consumed, err := oauthSvc.ConsumeCode(ctx, oauth.ConsumeCodeParams{
		Code:         code,
		ClientID:     client.ID,
		RedirectURI:  "http://localhost:9999/cb",
		CodeVerifier: verifier,
	})
	if err != nil {
		t.Fatalf("consume code: %v", err)
	}
	if consumed.UserID != userID {
		t.Errorf("user id: got %s want %s", consumed.UserID, userID)
	}
	if consumed.ProjectID != projectID {
		t.Errorf("project id: got %s want %s", consumed.ProjectID, projectID)
	}
}

func TestConsumeCode_OneShot(t *testing.T) {
	oauthSvc, authSvc, _ := fixture(t)
	userID, projectID := seedUserAndProject(t, authSvc, "alice@example.com")
	client := registerClient(t, oauthSvc, "http://localhost:9999/cb")
	verifier, challenge := pkcePair(t)
	ctx := context.Background()

	code, err := oauthSvc.IssueCode(ctx, oauth.IssueCodeParams{
		ClientID:            client.ID,
		UserID:              userID,
		ProjectID:           projectID,
		RedirectURI:         "http://localhost:9999/cb",
		Scope:               oauth.ScopeAPI,
		CodeChallenge:       challenge,
		CodeChallengeMethod: oauth.CodeChallengeMethodS256,
	})
	if err != nil {
		t.Fatalf("issue code: %v", err)
	}
	if _, err := oauthSvc.ConsumeCode(ctx, oauth.ConsumeCodeParams{
		Code: code, ClientID: client.ID, RedirectURI: "http://localhost:9999/cb", CodeVerifier: verifier,
	}); err != nil {
		t.Fatalf("first consume: %v", err)
	}
	_, err = oauthSvc.ConsumeCode(ctx, oauth.ConsumeCodeParams{
		Code: code, ClientID: client.ID, RedirectURI: "http://localhost:9999/cb", CodeVerifier: verifier,
	})
	if !errors.Is(err, oauth.ErrInvalidGrant) {
		t.Errorf("second consume: got %v want ErrInvalidGrant", err)
	}
}

func TestConsumeCode_MismatchedClient(t *testing.T) {
	oauthSvc, authSvc, _ := fixture(t)
	userID, projectID := seedUserAndProject(t, authSvc, "alice@example.com")
	clientA := registerClient(t, oauthSvc, "http://localhost:9999/cb")
	clientB := registerClient(t, oauthSvc, "http://localhost:9999/cb")
	verifier, challenge := pkcePair(t)
	ctx := context.Background()

	code, _ := oauthSvc.IssueCode(ctx, oauth.IssueCodeParams{
		ClientID:            clientA.ID,
		UserID:              userID,
		ProjectID:           projectID,
		RedirectURI:         "http://localhost:9999/cb",
		Scope:               oauth.ScopeAPI,
		CodeChallenge:       challenge,
		CodeChallengeMethod: oauth.CodeChallengeMethodS256,
	})
	_, err := oauthSvc.ConsumeCode(ctx, oauth.ConsumeCodeParams{
		Code: code, ClientID: clientB.ID, RedirectURI: "http://localhost:9999/cb", CodeVerifier: verifier,
	})
	if !errors.Is(err, oauth.ErrInvalidGrant) {
		t.Errorf("got %v want ErrInvalidGrant", err)
	}
}

func TestConsumeCode_MismatchedRedirectURI(t *testing.T) {
	oauthSvc, authSvc, _ := fixture(t)
	userID, projectID := seedUserAndProject(t, authSvc, "alice@example.com")
	client := registerClient(t, oauthSvc, "http://localhost:9999/cb")
	verifier, challenge := pkcePair(t)
	ctx := context.Background()

	code, _ := oauthSvc.IssueCode(ctx, oauth.IssueCodeParams{
		ClientID:            client.ID,
		UserID:              userID,
		ProjectID:           projectID,
		RedirectURI:         "http://localhost:9999/cb",
		Scope:               oauth.ScopeAPI,
		CodeChallenge:       challenge,
		CodeChallengeMethod: oauth.CodeChallengeMethodS256,
	})
	_, err := oauthSvc.ConsumeCode(ctx, oauth.ConsumeCodeParams{
		Code: code, ClientID: client.ID, RedirectURI: "http://localhost:9999/wrong", CodeVerifier: verifier,
	})
	if !errors.Is(err, oauth.ErrInvalidGrant) {
		t.Errorf("got %v want ErrInvalidGrant", err)
	}
}

func TestConsumeCode_PKCEMismatch(t *testing.T) {
	oauthSvc, authSvc, _ := fixture(t)
	userID, projectID := seedUserAndProject(t, authSvc, "alice@example.com")
	client := registerClient(t, oauthSvc, "http://localhost:9999/cb")
	_, challenge := pkcePair(t)
	ctx := context.Background()

	code, _ := oauthSvc.IssueCode(ctx, oauth.IssueCodeParams{
		ClientID:            client.ID,
		UserID:              userID,
		ProjectID:           projectID,
		RedirectURI:         "http://localhost:9999/cb",
		Scope:               oauth.ScopeAPI,
		CodeChallenge:       challenge,
		CodeChallengeMethod: oauth.CodeChallengeMethodS256,
	})
	_, err := oauthSvc.ConsumeCode(ctx, oauth.ConsumeCodeParams{
		Code: code, ClientID: client.ID, RedirectURI: "http://localhost:9999/cb",
		CodeVerifier: strings.Repeat("x", 43),
	})
	if !errors.Is(err, oauth.ErrInvalidGrant) {
		t.Errorf("got %v want ErrInvalidGrant", err)
	}
}

func TestConsumeCode_Expired(t *testing.T) {
	oauthSvc, authSvc, _ := fixture(t)
	userID, projectID := seedUserAndProject(t, authSvc, "alice@example.com")
	client := registerClient(t, oauthSvc, "http://localhost:9999/cb")
	verifier, challenge := pkcePair(t)
	// Issue codes with the clock 30 minutes in the past — by the time we
	// consume (real now), the expires_at has elapsed.
	oauthSvc.SetNow(func() time.Time { return time.Now().Add(-30 * time.Minute) })
	ctx := context.Background()
	code, _ := oauthSvc.IssueCode(ctx, oauth.IssueCodeParams{
		ClientID:            client.ID,
		UserID:              userID,
		ProjectID:           projectID,
		RedirectURI:         "http://localhost:9999/cb",
		Scope:               oauth.ScopeAPI,
		CodeChallenge:       challenge,
		CodeChallengeMethod: oauth.CodeChallengeMethodS256,
	})
	oauthSvc.SetNow(time.Now)
	_, err := oauthSvc.ConsumeCode(ctx, oauth.ConsumeCodeParams{
		Code: code, ClientID: client.ID, RedirectURI: "http://localhost:9999/cb", CodeVerifier: verifier,
	})
	if !errors.Is(err, oauth.ErrInvalidGrant) {
		t.Errorf("got %v want ErrInvalidGrant", err)
	}
}
