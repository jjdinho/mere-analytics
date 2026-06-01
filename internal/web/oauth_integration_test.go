package web_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jjdinho/mere-analytics/internal/auth"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/internal/web"
	"github.com/jjdinho/mere-analytics/migrations"
)

// startOAuthStack is startStack's OAuth-aware sibling — same plumbing plus an
// oauth.Service wired into the web handler.
func startOAuthStack(t *testing.T) (*httptest.Server, *auth.Service, *oauth.Service) {
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
	srv := httptest.NewServer(web.Handler(web.Options{
		AuthService:   authSvc,
		OAuthService:  oauthSvc,
		OAuthIssuer:   "https://issuer.test",
		Logger:        logger,
		SecureCookies: false,
	}))
	t.Cleanup(srv.Close)
	return srv, authSvc, oauthSvc
}

func registerOAuthClient(t *testing.T, srv *httptest.Server, redirect string) string {
	t.Helper()
	body := bytes.NewBufferString(`{"client_name":"cli","redirect_uris":["` + redirect + `"]}`)
	resp, err := http.Post(srv.URL+"/oauth/register", "application/json", body)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("register status: %d body=%s", resp.StatusCode, raw)
	}
	var payload struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.ClientID == "" {
		t.Fatalf("empty client_id")
	}
	return payload.ClientID
}

func seedOAuthUserAndProject(t *testing.T, srv *httptest.Server, svc *auth.Service, email string) (*http.Client, string, string) {
	t.Helper()
	c := seedAndLogin(t, srv, svc, email)
	_, body := mustGet(t, c, srv.URL+"/")
	teamID := findIDFromHref(t, body, "/teams/")
	resp := formPostExpect(t, c, srv, "/", "/teams/"+teamID+"/projects", url.Values{"name": {"prod"}})
	projectURL := resp.Header.Get("Location")
	resp.Body.Close()
	projectID := strings.TrimPrefix(projectURL, "/projects/")
	return c, teamID, projectID
}

func newPKCEPair(t *testing.T) (verifier, challenge string) {
	t.Helper()
	verifier = "verifier-" + strings.Repeat("a", 35)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

func authorizeURL(srvURL, clientID, redirect, state, challenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirect)
	q.Set("scope", oauth.ScopeAPI)
	q.Set("state", state)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	return srvURL + "/oauth/authorize?" + q.Encode()
}

func TestOAuthMetadata_DiscoveryDocument(t *testing.T) {
	srv, _, _ := startOAuthStack(t)
	resp, err := http.Get(srv.URL + "/.well-known/oauth-authorization-server")
	if err != nil {
		t.Fatalf("get metadata: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, k := range []string{"issuer", "authorization_endpoint", "token_endpoint", "registration_endpoint", "code_challenge_methods_supported"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("metadata missing %q", k)
		}
	}
}

func TestOAuth_AuthorizeRequiresLogin(t *testing.T) {
	srv, _, _ := startOAuthStack(t)
	clientID := registerOAuthClient(t, srv, "http://localhost:9999/cb")
	_, challenge := newPKCEPair(t)

	c := clientWithJar(t)
	resp, err := c.Get(authorizeURL(srv.URL, clientID, "http://localhost:9999/cb", "xyz", challenge))
	if err != nil {
		t.Fatalf("get authorize: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status: %d want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/login?next=") {
		t.Errorf("Location: %q want /login?next=…", loc)
	}
}

func TestOAuth_HappyPath_AuthorizeApproveToken(t *testing.T) {
	srv, authSvc, _ := startOAuthStack(t)
	c, _, projectID := seedOAuthUserAndProject(t, srv, authSvc, "alice@example.com")
	clientID := registerOAuthClient(t, srv, "http://localhost:9999/cb")
	verifier, challenge := newPKCEPair(t)

	resp, body := mustGet(t, c, authorizeURL(srv.URL, clientID, "http://localhost:9999/cb", "xyz", challenge))
	if resp.StatusCode != 200 {
		t.Fatalf("authorize status: %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(body, "Approve") {
		t.Errorf("consent page missing Approve button:\n%s", body)
	}
	csrf := extractCSRFToken(t, body)

	form := url.Values{
		"csrf_token":            {csrf},
		"client_id":             {clientID},
		"redirect_uri":          {"http://localhost:9999/cb"},
		"state":                 {"xyz"},
		"scope":                 {oauth.ScopeAPI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
		"decision":              {"approve"},
		"project_id":            {projectID},
	}
	approveResp, err := c.PostForm(srv.URL+"/oauth/authorize", form)
	if err != nil {
		t.Fatalf("post authorize: %v", err)
	}
	defer approveResp.Body.Close()
	if approveResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("authorize POST status: %d", approveResp.StatusCode)
	}
	loc, err := url.Parse(approveResp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if loc.Host != "localhost:9999" {
		t.Errorf("Location host: %q want localhost:9999", loc.Host)
	}
	code := loc.Query().Get("code")
	if code == "" {
		t.Fatalf("no code in redirect: %s", loc.String())
	}
	if loc.Query().Get("state") != "xyz" {
		t.Errorf("state echo missing: %s", loc.String())
	}

	// Token exchange.
	tokenForm := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://localhost:9999/cb"},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	tokenResp, err := http.PostForm(srv.URL+"/oauth/token", tokenForm)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	tokenBody, _ := io.ReadAll(tokenResp.Body)
	tokenResp.Body.Close()
	if tokenResp.StatusCode != 200 {
		t.Fatalf("token status: %d body=%s", tokenResp.StatusCode, tokenBody)
	}
	var tokenPayload struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(tokenBody, &tokenPayload); err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if tokenPayload.AccessToken == "" || tokenPayload.TokenType != "Bearer" || tokenPayload.Scope != oauth.ScopeAPI {
		t.Errorf("token payload: %+v", tokenPayload)
	}

	// Second token exchange with the same code → invalid_grant (one-shot).
	resp2, err := http.PostForm(srv.URL+"/oauth/token", tokenForm)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Errorf("replay status: %d want 400", resp2.StatusCode)
	}

	// /v1/whoami with the bearer token.
	req, _ := http.NewRequest("GET", srv.URL+"/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+tokenPayload.AccessToken)
	whoResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	whoBody, _ := io.ReadAll(whoResp.Body)
	whoResp.Body.Close()
	if whoResp.StatusCode != 200 {
		t.Fatalf("whoami status: %d body=%s", whoResp.StatusCode, whoBody)
	}
	var who map[string]any
	if err := json.Unmarshal(whoBody, &who); err != nil {
		t.Fatalf("decode whoami: %v", err)
	}
	if who["project_id"] != projectID {
		t.Errorf("project_id mismatch: %v want %s", who["project_id"], projectID)
	}
}

func TestOAuth_TamperedVerifier(t *testing.T) {
	srv, authSvc, _ := startOAuthStack(t)
	c, _, projectID := seedOAuthUserAndProject(t, srv, authSvc, "alice@example.com")
	clientID := registerOAuthClient(t, srv, "http://localhost:9999/cb")
	_, challenge := newPKCEPair(t)

	_, body := mustGet(t, c, authorizeURL(srv.URL, clientID, "http://localhost:9999/cb", "s", challenge))
	csrf := extractCSRFToken(t, body)
	approveResp, err := c.PostForm(srv.URL+"/oauth/authorize", url.Values{
		"csrf_token":            {csrf},
		"client_id":             {clientID},
		"redirect_uri":          {"http://localhost:9999/cb"},
		"state":                 {"s"},
		"scope":                 {oauth.ScopeAPI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
		"decision":              {"approve"},
		"project_id":            {projectID},
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	defer approveResp.Body.Close()
	loc, _ := url.Parse(approveResp.Header.Get("Location"))
	code := loc.Query().Get("code")

	resp, _ := http.PostForm(srv.URL+"/oauth/token", url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"http://localhost:9999/cb"},
		"client_id":     {clientID},
		"code_verifier": {strings.Repeat("z", 43)},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: %d want 400", resp.StatusCode)
	}
}

func TestOAuth_Whoami_UnauthorizedCases(t *testing.T) {
	srv, _, _ := startOAuthStack(t)

	cases := []struct {
		name, header string
	}{
		{"no header", ""},
		{"garbage bearer", "Bearer garbage-token"},
		{"public token prefix", "Bearer " + auth.PublicTokenPrefix + strings.Repeat("a", 43)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", srv.URL+"/v1/whoami", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("whoami: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status: %d want 401", resp.StatusCode)
			}
		})
	}
}

func TestOAuth_DenyFlow(t *testing.T) {
	srv, authSvc, _ := startOAuthStack(t)
	c, _, projectID := seedOAuthUserAndProject(t, srv, authSvc, "alice@example.com")
	clientID := registerOAuthClient(t, srv, "http://localhost:9999/cb")
	_, challenge := newPKCEPair(t)

	_, body := mustGet(t, c, authorizeURL(srv.URL, clientID, "http://localhost:9999/cb", "abc", challenge))
	csrf := extractCSRFToken(t, body)
	resp, _ := c.PostForm(srv.URL+"/oauth/authorize", url.Values{
		"csrf_token":            {csrf},
		"client_id":             {clientID},
		"redirect_uri":          {"http://localhost:9999/cb"},
		"state":                 {"abc"},
		"scope":                 {oauth.ScopeAPI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
		"decision":              {"deny"},
		"project_id":            {projectID},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status: %d want 303", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Query().Get("error") != "access_denied" {
		t.Errorf("error code: %q want access_denied", loc.Query().Get("error"))
	}
	if loc.Query().Get("state") != "abc" {
		t.Errorf("state echo missing on deny")
	}
}

func TestOAuth_UnknownClient_NoRedirect(t *testing.T) {
	srv, _, _ := startOAuthStack(t)
	_, challenge := newPKCEPair(t)

	resp, err := http.Get(authorizeURL(srv.URL, "00000000-0000-0000-0000-000000000000", "http://localhost:9999/cb", "x", challenge))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: %d want 400", resp.StatusCode)
	}
}

func TestOAuth_UnregisteredRedirect_NoRedirect(t *testing.T) {
	srv, _, _ := startOAuthStack(t)
	clientID := registerOAuthClient(t, srv, "http://localhost:9999/cb")
	_, challenge := newPKCEPair(t)

	resp, err := http.Get(authorizeURL(srv.URL, clientID, "http://attacker.example/cb", "x", challenge))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: %d want 400", resp.StatusCode)
	}
}

func TestOAuth_ApproveForNonMemberProject_Forbidden(t *testing.T) {
	srv, authSvc, _ := startOAuthStack(t)
	cAlice, _, _ := seedOAuthUserAndProject(t, srv, authSvc, "alice@example.com")
	cBob, _, bobProjectID := seedOAuthUserAndProject(t, srv, authSvc, "bob@example.com")
	_ = cBob

	clientID := registerOAuthClient(t, srv, "http://localhost:9999/cb")
	_, challenge := newPKCEPair(t)

	_, body := mustGet(t, cAlice, authorizeURL(srv.URL, clientID, "http://localhost:9999/cb", "x", challenge))
	csrf := extractCSRFToken(t, body)

	// Alice approves but supplies Bob's project_id.
	resp, err := cAlice.PostForm(srv.URL+"/oauth/authorize", url.Values{
		"csrf_token":            {csrf},
		"client_id":             {clientID},
		"redirect_uri":          {"http://localhost:9999/cb"},
		"state":                 {"x"},
		"scope":                 {oauth.ScopeAPI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
		"decision":              {"approve"},
		"project_id":            {bobProjectID},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: %d want 403", resp.StatusCode)
	}
}

// TestOAuth_RedirectURIWithQueryString_AppendsCodeWithAmpersand registers a
// client whose redirect_uri already carries a query string (RFC 6749 §3.1.2
// allows this) and confirms the success redirect joins `code`/`state` with
// `&`, not a second `?`. Regression guard for the buggy "?"-only join.
func TestOAuth_RedirectURIWithQueryString_AppendsCodeWithAmpersand(t *testing.T) {
	srv, authSvc, _ := startOAuthStack(t)
	c, _, projectID := seedOAuthUserAndProject(t, srv, authSvc, "alice@example.com")
	redirect := "http://localhost:9999/cb?env=prod"
	clientID := registerOAuthClient(t, srv, redirect)
	_, challenge := newPKCEPair(t)

	_, body := mustGet(t, c, authorizeURL(srv.URL, clientID, redirect, "xyz", challenge))
	csrf := extractCSRFToken(t, body)
	resp, err := c.PostForm(srv.URL+"/oauth/authorize", url.Values{
		"csrf_token":            {csrf},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"state":                 {"xyz"},
		"scope":                 {oauth.ScopeAPI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
		"decision":              {"approve"},
		"project_id":            {projectID},
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status: %d want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if strings.Count(loc, "?") != 1 {
		t.Errorf("Location has multiple '?': %s", loc)
	}
	parsed, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if parsed.Query().Get("env") != "prod" {
		t.Errorf("env query lost: %s", loc)
	}
	if parsed.Query().Get("code") == "" {
		t.Errorf("code missing: %s", loc)
	}
	if parsed.Query().Get("state") != "xyz" {
		t.Errorf("state missing: %s", loc)
	}
}

// TestOAuth_RedirectURIWithQueryString_ErrorAlsoUsesAmpersand mirrors the
// success-path test for the error redirect: a deny decision with a query-
// string redirect URI must produce a single-? URL with `error`/`state`
// joined by `&`.
func TestOAuth_RedirectURIWithQueryString_ErrorAlsoUsesAmpersand(t *testing.T) {
	srv, authSvc, _ := startOAuthStack(t)
	c, _, projectID := seedOAuthUserAndProject(t, srv, authSvc, "alice@example.com")
	redirect := "http://localhost:9999/cb?env=prod"
	clientID := registerOAuthClient(t, srv, redirect)
	_, challenge := newPKCEPair(t)

	_, body := mustGet(t, c, authorizeURL(srv.URL, clientID, redirect, "abc", challenge))
	csrf := extractCSRFToken(t, body)
	resp, err := c.PostForm(srv.URL+"/oauth/authorize", url.Values{
		"csrf_token":            {csrf},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"state":                 {"abc"},
		"scope":                 {oauth.ScopeAPI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"response_type":         {"code"},
		"decision":              {"deny"},
		"project_id":            {projectID},
	})
	if err != nil {
		t.Fatalf("deny: %v", err)
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if strings.Count(loc, "?") != 1 {
		t.Errorf("Location has multiple '?': %s", loc)
	}
	parsed, _ := url.Parse(loc)
	if parsed.Query().Get("error") != "access_denied" {
		t.Errorf("error code missing: %s", loc)
	}
	if parsed.Query().Get("env") != "prod" {
		t.Errorf("env query lost: %s", loc)
	}
}
