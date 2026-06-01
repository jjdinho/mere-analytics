package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/mcp"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/internal/web"
	"github.com/jjdinho/mere-analytics/migrations"
)

const mcpSentinelBody = "mcp-handler-reached"

// TestMCP_RequiresBearer proves /mcp is mounted behind the same RequireBearer
// middleware as /api/v1/*: a missing or bogus token is rejected with 401
// before the MCP handler ever runs, and only a valid OAuth access token is let
// through to it. A sentinel handler (not the real transport) stands in for the
// mounted /mcp handler so the test stays Postgres-only.
func TestMCP_RequiresBearer(t *testing.T) {
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
	authSvc := auth.NewService(pool)
	oauthSvc := oauth.NewService(pool)

	sentinel := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(mcpSentinelBody))
	})
	srv := httptest.NewServer(web.Handler(web.Options{
		AuthService:  authSvc,
		OAuthService: oauthSvc,
		OAuthIssuer:  "https://issuer.test",
		Logger:       logger,
		MCPHandler:   sentinel,
	}))
	t.Cleanup(srv.Close)

	post := func(t *testing.T, authHeader string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp",
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Content-Type", "application/json")
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}

	t.Run("no_token_401", func(t *testing.T) {
		resp, body := post(t, "")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if resp.Header.Get("WWW-Authenticate") == "" {
			t.Errorf("missing WWW-Authenticate header")
		}
		if strings.Contains(body, mcpSentinelBody) {
			t.Errorf("MCP handler reached without a token")
		}
	})

	t.Run("bogus_token_401", func(t *testing.T) {
		resp, body := post(t, "Bearer not-a-real-token")
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", resp.StatusCode)
		}
		if strings.Contains(body, mcpSentinelBody) {
			t.Errorf("MCP handler reached with a bogus token")
		}
	})

	t.Run("valid_token_reaches_handler", func(t *testing.T) {
		signup, err := authSvc.Signup(ctx, auth.SignupRequest{Email: "bob@example.com", Password: "correct-horse-battery"})
		if err != nil {
			t.Fatalf("signup: %v", err)
		}
		proj, err := auth.NewViewer(authSvc, signup.User.ID).Projects(ctx).Create(signup.Team.ID, "prod")
		if err != nil {
			t.Fatalf("create project: %v", err)
		}
		client, err := oauthSvc.RegisterClient(ctx, oauth.RegisterParams{
			Name:         "cli",
			RedirectURIs: []string{"http://localhost:9999/cb"},
		})
		if err != nil {
			t.Fatalf("register client: %v", err)
		}
		token, _, err := oauthSvc.IssueAccessToken(ctx, oauth.IssueAccessTokenParams{
			ClientID:  client.ID,
			UserID:    signup.User.ID,
			ProjectID: proj.ID,
			Scope:     oauth.ScopeAPI,
		})
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}

		resp, body := post(t, "Bearer "+token)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, body)
		}
		if !strings.Contains(body, mcpSentinelBody) {
			t.Errorf("valid token did not reach the MCP handler; body=%s", body)
		}
	})
}

func TestMCP_RequestBodyIsCapped(t *testing.T) {
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
	authSvc := auth.NewService(pool)
	oauthSvc := oauth.NewService(pool)

	srv := httptest.NewServer(web.Handler(web.Options{
		AuthService:       authSvc,
		OAuthService:      oauthSvc,
		OAuthIssuer:       "https://issuer.test",
		Logger:            logger,
		QueryMaxBodyBytes: 32,
		MCPHandler:        mcp.NewHTTPHandler(mcp.Deps{Logger: logger}),
	}))
	t.Cleanup(srv.Close)

	signup, err := authSvc.Signup(ctx, auth.SignupRequest{Email: "carol@example.com", Password: "correct-horse-battery"})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	proj, err := auth.NewViewer(authSvc, signup.User.ID).Projects(ctx).Create(signup.Team.ID, "prod")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	client, err := oauthSvc.RegisterClient(ctx, oauth.RegisterParams{
		Name:         "cli",
		RedirectURIs: []string{"http://localhost:9999/cb"},
	})
	if err != nil {
		t.Fatalf("register client: %v", err)
	}
	token, _, err := oauthSvc.IssueAccessToken(ctx, oauth.IssueAccessTokenParams{
		ClientID:  client.ID,
		UserID:    signup.User.ID,
		ProjectID: proj.ID,
		Scope:     oauth.ScopeAPI,
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(strings.Repeat("x", 128)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "request body too large") {
		t.Fatalf("body cap did not fire; response=%s", body)
	}
}
