package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jjdinho/mere-analytics/internal/auth"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/internal/web"
	"github.com/jjdinho/mere-analytics/migrations"
)

// startStack spins up Postgres, runs migrations, and returns the HTTP test
// server alongside the *auth.Service so tests can seed users directly via
// svc.Signup (the public /signup route was removed; user creation in
// production goes through scripts/operator/create-user.sql or the anon-
// invite POST).
func startStack(t *testing.T) (*httptest.Server, *auth.Service) {
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

	svc := auth.NewService(pool)
	srv := httptest.NewServer(web.Handler(web.Options{
		AuthService:   svc,
		Logger:        logger,
		SecureCookies: false,
	}))
	t.Cleanup(srv.Close)
	return srv, svc
}

// clientWithJar returns an http.Client with cookie storage and redirects
// disabled so test bodies can inspect redirect responses directly.
func clientWithJar(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// extractCSRFToken parses the hidden csrf_token input from an HTML form body.
func extractCSRFToken(t *testing.T, body string) string {
	t.Helper()
	const marker = `name="csrf_token" value="`
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("csrf_token field missing in body:\n%s", body)
	}
	rest := body[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("malformed csrf_token field in body")
	}
	return rest[:j]
}

func mustGet(t *testing.T, c *http.Client, url string) (*http.Response, string) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(body)
}

// loginAs performs the full /login flow (CSRF harvest + form POST) and
// returns the cookied client. Fails the test on any non-303 response —
// callers are seeding for a happy-path scenario.
func loginAs(t *testing.T, srv *httptest.Server, email, password string) *http.Client {
	t.Helper()
	c := clientWithJar(t)
	_, body := mustGet(t, c, srv.URL+"/login")
	tok := extractCSRFToken(t, body)
	form := url.Values{"email": {email}, "password": {password}, "csrf_token": {tok}}
	resp, err := c.PostForm(srv.URL+"/login", form)
	if err != nil {
		t.Fatalf("login %s: %v", email, err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login %s: status %d, want 303", email, resp.StatusCode)
	}
	return c
}

// seedAndLogin creates the user via svc.Signup and returns a cookied client
// that's already logged in. Used by tests that need an authenticated viewer
// but don't care about exercising the create-account path.
func seedAndLogin(t *testing.T, srv *httptest.Server, svc *auth.Service, email string) *http.Client {
	t.Helper()
	const pw = "correct horse battery staple"
	if _, err := svc.Signup(context.Background(), auth.SignupRequest{Email: email, Password: pw}); err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
	return loginAs(t, srv, email, pw)
}

func TestSeededUser_canLoginAndSeeHome(t *testing.T) {
	srv, svc := startStack(t)
	c := seedAndLogin(t, srv, svc, "alice@example.com")

	homeResp, homeBody := mustGet(t, c, srv.URL+"/")
	if homeResp.StatusCode != 200 {
		t.Fatalf("GET / after login: %d", homeResp.StatusCode)
	}
	if !strings.Contains(homeBody, "alice@example.com") {
		t.Errorf("home does not show email: %s", homeBody)
	}
	if !strings.Contains(homeBody, "Welcome") {
		t.Errorf("home does not show welcome: %s", homeBody)
	}
}

func TestLogin_setsCookie_LogoutDestroys(t *testing.T) {
	srv, svc := startStack(t)

	// Seed a user directly (no public /signup), then exercise the real
	// /login + /logout flows.
	if _, err := svc.Signup(context.Background(), auth.SignupRequest{
		Email: "bob@example.com", Password: "correct horse battery staple",
	}); err != nil {
		t.Fatalf("seed bob: %v", err)
	}

	c := clientWithJar(t)
	_, loginBody := mustGet(t, c, srv.URL+"/login")
	tok := extractCSRFToken(t, loginBody)

	loginForm := url.Values{"email": {"bob@example.com"}, "password": {"correct horse battery staple"}, "csrf_token": {tok}}
	loginResp, err := c.PostForm(srv.URL+"/login", loginForm)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	io.Copy(io.Discard, loginResp.Body)
	loginResp.Body.Close()
	if loginResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login status: %d want 303", loginResp.StatusCode)
	}
	if !hasSessionCookie(t, c, srv.URL) {
		t.Fatalf("no session cookie after login")
	}

	// Logout → cookie cleared.
	tok = mustCSRFFromHome(t, c, srv.URL)
	logoutResp, _ := c.PostForm(srv.URL+"/logout", url.Values{"csrf_token": {tok}})
	logoutResp.Body.Close()
	assertNoSessionCookie(t, c, srv.URL)
}

func TestLogin_invalidCreds_returnsUnauthorized_NoCookie(t *testing.T) {
	srv, _ := startStack(t)
	c := clientWithJar(t)

	_, body := mustGet(t, c, srv.URL+"/login")
	tok := extractCSRFToken(t, body)

	form := url.Values{"email": {"ghost@example.com"}, "password": {"correct horse battery staple"}, "csrf_token": {tok}}
	resp, err := c.PostForm(srv.URL+"/login", form)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status: %d want 401", resp.StatusCode)
	}
	if !strings.Contains(string(respBody), "Invalid credentials") {
		t.Errorf("body missing error message: %s", respBody)
	}
	if hasSessionCookie(t, c, srv.URL) {
		t.Errorf("login failure left a session cookie")
	}
}

// TestCSRF_missingToken_returns403 verifies the anon /login POST is
// CSRF-protected (the same middleware also guards the anon-invite POST).
func TestCSRF_missingToken_returns403(t *testing.T) {
	srv, _ := startStack(t)
	c := clientWithJar(t)

	// Prime the anon csrf cookie via GET.
	_, _ = mustGet(t, c, srv.URL+"/login")

	form := url.Values{"email": {"x@example.com"}, "password": {"correct horse battery staple"}}
	resp, err := c.PostForm(srv.URL+"/login", form)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: %d want 403", resp.StatusCode)
	}
}

func TestCSRF_wrongToken_returns403(t *testing.T) {
	srv, _ := startStack(t)
	c := clientWithJar(t)

	_, _ = mustGet(t, c, srv.URL+"/login")

	form := url.Values{"email": {"x@example.com"}, "password": {"correct horse battery staple"}, "csrf_token": {"forgery"}}
	resp, err := c.PostForm(srv.URL+"/login", form)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: %d want 403", resp.StatusCode)
	}
}

// TestHomeEscapesEmail asserts templ escapes attacker-controlled email
// content. Previously seeded via /signup; now seeded directly via
// svc.Signup since the public route is gone. The XSS payload still passes
// our lax email validation (single quote / angle brackets are allowed in
// the local part).
func TestHomeEscapesEmail(t *testing.T) {
	srv, svc := startStack(t)
	const xssEmail = `<img src=x>@evil.example`
	if _, err := svc.Signup(context.Background(), auth.SignupRequest{
		Email: xssEmail, Password: "correct horse battery staple",
	}); err != nil {
		t.Fatalf("seed xss user: %v", err)
	}
	c := loginAs(t, srv, xssEmail, "correct horse battery staple")

	_, home := mustGet(t, c, srv.URL+"/")
	if strings.Contains(home, "<img src=x>") {
		t.Errorf("templ did not escape email: %s", home)
	}
	if !strings.Contains(home, "&lt;img src=x&gt;") {
		t.Errorf("escaped form missing: %s", home)
	}
}

func mustCSRFFromHome(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	// Use the logout form on the layout to harvest a token. If user is not
	// authenticated, /login will do.
	resp, err := c.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return extractCSRFToken(t, string(body))
}

func hasSessionCookie(t *testing.T, c *http.Client, base string) bool {
	t.Helper()
	u, _ := url.Parse(base)
	for _, ck := range c.Jar.Cookies(u) {
		if ck.Name == auth.SessionCookieName && ck.Value != "" {
			return true
		}
	}
	return false
}

func assertNoSessionCookie(t *testing.T, c *http.Client, base string) {
	t.Helper()
	if hasSessionCookie(t, c, base) {
		t.Errorf("expected no session cookie, got one")
	}
}
