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

func startStack(t *testing.T) *httptest.Server {
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
	return srv
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

func TestSignup_thenAuthenticatedHome(t *testing.T) {
	srv := startStack(t)
	c := clientWithJar(t)

	// GET /signup → 200, returns a CSRF token in the form.
	resp, body := mustGet(t, c, srv.URL+"/signup")
	if resp.StatusCode != 200 {
		t.Fatalf("GET /signup: %d", resp.StatusCode)
	}
	token := extractCSRFToken(t, body)

	form := url.Values{}
	form.Set("email", "alice@example.com")
	form.Set("password", "correct horse battery staple")
	form.Set("csrf_token", token)

	postResp, err := c.PostForm(srv.URL+"/signup", form)
	if err != nil {
		t.Fatalf("POST /signup: %v", err)
	}
	io.Copy(io.Discard, postResp.Body)
	postResp.Body.Close()
	if postResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /signup: status %d, want 303", postResp.StatusCode)
	}

	// Follow to /, expect logged-in home with email rendered.
	homeResp, homeBody := mustGet(t, c, srv.URL+"/")
	if homeResp.StatusCode != 200 {
		t.Fatalf("GET / after signup: %d", homeResp.StatusCode)
	}
	if !strings.Contains(homeBody, "alice@example.com") {
		t.Errorf("home does not show email: %s", homeBody)
	}
	if !strings.Contains(homeBody, "Welcome") {
		t.Errorf("home does not show welcome: %s", homeBody)
	}
}

func TestLogin_setsCookie_LogoutDestroys(t *testing.T) {
	srv := startStack(t)
	c := clientWithJar(t)

	// Sign up first to have a user.
	_, body := mustGet(t, c, srv.URL+"/signup")
	tok := extractCSRFToken(t, body)
	signup := url.Values{"email": {"bob@example.com"}, "password": {"correct horse battery staple"}, "csrf_token": {tok}}
	signupResp, _ := c.PostForm(srv.URL+"/signup", signup)
	signupResp.Body.Close()

	// Logout to clear the post-signup session.
	logoutToken := mustCSRFFromHome(t, c, srv.URL)
	logoutResp, _ := c.PostForm(srv.URL+"/logout", url.Values{"csrf_token": {logoutToken}})
	logoutResp.Body.Close()
	assertNoSessionCookie(t, c, srv.URL)

	// GET /login → form with CSRF.
	_, loginBody := mustGet(t, c, srv.URL+"/login")
	tok = extractCSRFToken(t, loginBody)

	// POST valid creds → 303 + session cookie.
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
	logoutResp2, _ := c.PostForm(srv.URL+"/logout", url.Values{"csrf_token": {tok}})
	logoutResp2.Body.Close()
	assertNoSessionCookie(t, c, srv.URL)
}

func TestLogin_invalidCreds_returnsUnauthorized_NoCookie(t *testing.T) {
	srv := startStack(t)
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

func TestCSRF_missingToken_returns403(t *testing.T) {
	srv := startStack(t)
	c := clientWithJar(t)

	// Prime the anon csrf cookie via GET.
	_, _ = mustGet(t, c, srv.URL+"/signup")

	form := url.Values{"email": {"x@example.com"}, "password": {"correct horse battery staple"}}
	resp, err := c.PostForm(srv.URL+"/signup", form)
	if err != nil {
		t.Fatalf("POST /signup: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: %d want 403", resp.StatusCode)
	}
}

func TestCSRF_wrongToken_returns403(t *testing.T) {
	srv := startStack(t)
	c := clientWithJar(t)

	_, _ = mustGet(t, c, srv.URL+"/signup")

	form := url.Values{"email": {"x@example.com"}, "password": {"correct horse battery staple"}, "csrf_token": {"forgery"}}
	resp, err := c.PostForm(srv.URL+"/signup", form)
	if err != nil {
		t.Fatalf("POST /signup: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: %d want 403", resp.StatusCode)
	}
}

func TestSignup_duplicateEmail_409(t *testing.T) {
	srv := startStack(t)
	c1 := clientWithJar(t)
	c2 := clientWithJar(t)

	_, body := mustGet(t, c1, srv.URL+"/signup")
	tok := extractCSRFToken(t, body)
	form := url.Values{"email": {"dup@example.com"}, "password": {"correct horse battery staple"}, "csrf_token": {tok}}
	resp, _ := c1.PostForm(srv.URL+"/signup", form)
	resp.Body.Close()

	// Second client tries same email.
	_, body2 := mustGet(t, c2, srv.URL+"/signup")
	tok2 := extractCSRFToken(t, body2)
	form2 := url.Values{"email": {"DUP@example.com"}, "password": {"correct horse battery staple"}, "csrf_token": {tok2}}
	resp2, _ := c2.PostForm(srv.URL+"/signup", form2)
	b2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusConflict {
		t.Errorf("second signup status: %d want 409", resp2.StatusCode)
	}
	if !strings.Contains(string(b2), "Email already registered") {
		t.Errorf("body missing duplicate-email message: %s", b2)
	}
}

func TestHomeEscapesEmail(t *testing.T) {
	srv := startStack(t)
	c := clientWithJar(t)

	_, body := mustGet(t, c, srv.URL+"/signup")
	tok := extractCSRFToken(t, body)
	// "<script>" in an email actually fails our @-check, so use a payload that
	// passes ValidateEmail but would XSS if templ didn't escape: a single quote
	// or angle bracket in the local part is allowed by our lax validator.
	xssEmail := `<img src=x>@evil.example`
	form := url.Values{"email": {xssEmail}, "password": {"correct horse battery staple"}, "csrf_token": {tok}}
	resp, _ := c.PostForm(srv.URL+"/signup", form)
	resp.Body.Close()

	_, home := mustGet(t, c, srv.URL+"/")
	if strings.Contains(home, "<img src=x>") {
		t.Errorf("templ did not escape email: %s", home)
	}
	// Must contain the escaped form.
	if !strings.Contains(home, "&lt;img src=x&gt;") {
		t.Errorf("escaped form missing: %s", home)
	}
}

func mustCSRFFromHome(t *testing.T, c *http.Client, base string) string {
	t.Helper()
	// Use the logout form on the layout to harvest a token. If user is not
	// authenticated, /signup will do.
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
