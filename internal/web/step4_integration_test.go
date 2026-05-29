package web_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jjdinho/mere-analytics/internal/auth"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/internal/web"
	"github.com/jjdinho/mere-analytics/migrations"
)

// signupClient creates a fresh user via the real signup flow, returning a
// cookied client + the user's email so tests can re-authenticate.
func signupClient(t *testing.T, srv *httptest.Server, email string) *http.Client {
	t.Helper()
	c := clientWithJar(t)
	_, body := mustGet(t, c, srv.URL+"/signup")
	tok := extractCSRFToken(t, body)
	form := url.Values{"email": {email}, "password": {"correct horse battery staple"}, "csrf_token": {tok}}
	resp, err := c.PostForm(srv.URL+"/signup", form)
	if err != nil {
		t.Fatalf("signup %s: %v", email, err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("signup %s: status %d want 303", email, resp.StatusCode)
	}
	return c
}

// findID scrapes a UUID from an href like /teams/<id> or /projects/<id> in
// rendered HTML. Used to discover the personal team id without poking PG.
func findIDFromHref(t *testing.T, body, prefix string) string {
	t.Helper()
	pat := regexp.MustCompile(regexp.QuoteMeta(prefix) + `([0-9a-f-]{36})`)
	m := pat.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no %s<uuid> found in body:\n%s", prefix, body)
	}
	return m[1]
}

// formPostExpect submits form values + csrf_token harvested from refURL,
// returning the response.
func formPostExpect(t *testing.T, c *http.Client, srv *httptest.Server, refURL, target string, vals url.Values) *http.Response {
	t.Helper()
	_, body := mustGet(t, c, srv.URL+refURL)
	vals.Set("csrf_token", extractCSRFToken(t, body))
	resp, err := c.PostForm(srv.URL+target, vals)
	if err != nil {
		t.Fatalf("POST %s: %v", target, err)
	}
	return resp
}

// TestHome_ListsTeamsAndProjects exercises the rebuilt /` page (Issue 5).
// Signup creates a personal team; the page should render it and let the user
// create a project, which then appears on the next render.
func TestHome_ListsTeamsAndProjects(t *testing.T) {
	srv := startStack(t)
	c := signupClient(t, srv, "alice@example.com")

	_, body := mustGet(t, c, srv.URL+"/")
	if !strings.Contains(body, "alice@example.com") {
		t.Errorf("home should show email, body: %s", body)
	}
	teamID := findIDFromHref(t, body, "/teams/")
	if teamID == "" {
		t.Fatalf("no team link on home")
	}

	resp := formPostExpect(t, c, srv, "/", "/teams/"+teamID+"/projects", url.Values{"name": {"prod"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create project status: %d want 303", resp.StatusCode)
	}

	_, body = mustGet(t, c, srv.URL+"/")
	if !strings.Contains(body, "prod") {
		t.Errorf("home should now list 'prod' project: %s", body)
	}
}

// TestProjectFlow_CreateTokenRevoke covers the project page UX: create a
// token (plaintext shown once), revoke it (gone from list), and verify the
// list NEVER includes the plaintext.
func TestProjectFlow_CreateTokenRevoke(t *testing.T) {
	srv := startStack(t)
	c := signupClient(t, srv, "alice@example.com")

	_, body := mustGet(t, c, srv.URL+"/")
	teamID := findIDFromHref(t, body, "/teams/")

	resp := formPostExpect(t, c, srv, "/", "/teams/"+teamID+"/projects", url.Values{"name": {"prod"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create project status: %d", resp.StatusCode)
	}
	projectURL := resp.Header.Get("Location")
	resp.Body.Close()

	// Create a token. Response is render-on-POST with plaintext.
	resp = formPostExpect(t, c, srv, projectURL, projectURL+"/tokens", url.Values{"name": {"ci"}})
	body2, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body2), auth.TokenPrefix) {
		t.Errorf("create-token response missing %q prefix:\n%s", auth.TokenPrefix, body2)
	}
	// Extract the plaintext for the leakage-assertion below.
	tokenPat := regexp.MustCompile(auth.TokenPrefix + `[A-Za-z0-9_-]{43}`)
	plain := tokenPat.FindString(string(body2))
	if plain == "" {
		t.Fatalf("could not locate plaintext token in response body")
	}

	// Subsequent GET to project page must NOT contain the plaintext (only
	// the name + revoke button); this is the negative leakage assertion.
	_, body3 := mustGet(t, c, srv.URL+projectURL)
	if strings.Contains(body3, plain) {
		t.Errorf("project page leaks plaintext token after creation: %s", body3)
	}
	if strings.Contains(body3, auth.TokenPrefix) {
		t.Errorf("project page still contains token prefix after creation: %s", body3)
	}

	// Revoke and verify the list updates.
	tokenID := findIDFromHref(t, body3, projectURL+"/tokens/")
	revResp := formPostExpect(t, c, srv, projectURL, projectURL+"/tokens/"+tokenID+"/revoke", url.Values{})
	revResp.Body.Close()
	if revResp.StatusCode != http.StatusSeeOther {
		t.Errorf("revoke status: %d want 303", revResp.StatusCode)
	}

	_, body4 := mustGet(t, c, srv.URL+projectURL)
	if strings.Contains(body4, tokenID) {
		t.Errorf("revoked token still listed: %s", body4)
	}
}

// TestProject_SoftDelete_then404 verifies a soft-deleted project's GET
// returns 404 — both for the owner and (implicitly) for everyone else.
func TestProject_SoftDelete_then404(t *testing.T) {
	srv := startStack(t)
	c := signupClient(t, srv, "alice@example.com")

	_, body := mustGet(t, c, srv.URL+"/")
	teamID := findIDFromHref(t, body, "/teams/")
	createResp := formPostExpect(t, c, srv, "/", "/teams/"+teamID+"/projects", url.Values{"name": {"prod"}})
	projectURL := createResp.Header.Get("Location")
	createResp.Body.Close()

	delResp := formPostExpect(t, c, srv, projectURL, projectURL+"/delete", url.Values{})
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status: %d want 303", delResp.StatusCode)
	}

	getResp, err := c.Get(srv.URL + projectURL)
	if err != nil {
		t.Fatalf("GET after soft-delete: %v", err)
	}
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusNotFound {
		t.Errorf("soft-deleted GET status: %d want 404", getResp.StatusCode)
	}
}

// TestCrossUserAuth_Matrix is the parameterized cross-user authorization
// test: user B's UUIDs must return 404 to user A on every protected route.
// This is the kernel security contract test for Step 4 — new routes that
// expose a {team_id} or {project_id} path param should be enrolled here.
func TestCrossUserAuth_Matrix(t *testing.T) {
	srv := startStack(t)
	cAlice := signupClient(t, srv, "alice@example.com")
	cBob := signupClient(t, srv, "bob@example.com")

	// Bob creates a team-owned project + token for the matrix.
	_, bobHome := mustGet(t, cBob, srv.URL+"/")
	bobTeamID := findIDFromHref(t, bobHome, "/teams/")
	pResp := formPostExpect(t, cBob, srv, "/", "/teams/"+bobTeamID+"/projects", url.Values{"name": {"bobs-thing"}})
	bobProjectURL := pResp.Header.Get("Location")
	pResp.Body.Close()
	bobProjectID := strings.TrimPrefix(bobProjectURL, "/projects/")

	tResp := formPostExpect(t, cBob, srv, bobProjectURL, bobProjectURL+"/tokens", url.Values{"name": {"bobs-token"}})
	tBody, _ := io.ReadAll(tResp.Body)
	tResp.Body.Close()
	bobTokenID := findIDFromHref(t, string(tBody), bobProjectURL+"/tokens/")

	// Matrix of (method, path) Alice tries against Bob's UUIDs.
	type row struct {
		method, path string
		body         url.Values
	}
	rows := []row{
		{"GET", "/teams/" + bobTeamID, nil},
		{"GET", "/projects/" + bobProjectID, nil},
		{"POST", "/teams/" + bobTeamID + "/invites", url.Values{}},
		{"POST", "/teams/" + bobTeamID + "/projects", url.Values{"name": {"sneaky"}}},
		{"POST", "/projects/" + bobProjectID + "/delete", url.Values{}},
		{"POST", "/projects/" + bobProjectID + "/tokens", url.Values{"name": {"sneaky"}}},
		{"POST", "/projects/" + bobProjectID + "/tokens/" + bobTokenID + "/revoke", url.Values{}},
	}
	for _, r := range rows {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			var resp *http.Response
			var err error
			if r.method == "GET" {
				resp, err = cAlice.Get(srv.URL + r.path)
			} else {
				// Harvest a CSRF token from Alice's home, then POST.
				_, body := mustGet(t, cAlice, srv.URL+"/")
				r.body.Set("csrf_token", extractCSRFToken(t, body))
				resp, err = cAlice.PostForm(srv.URL+r.path, r.body)
			}
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Errorf("status: got %d want 404", resp.StatusCode)
			}
		})
	}
}

// TestInviteFlow_AnonSignsUpToJoin walks the full anon→signup→join chain
// (Issue 8 + 12 strict).
func TestInviteFlow_AnonSignsUpToJoin(t *testing.T) {
	srv := startStack(t)
	cAlice := signupClient(t, srv, "alice@example.com")

	_, body := mustGet(t, cAlice, srv.URL+"/")
	teamID := findIDFromHref(t, body, "/teams/")
	invResp := formPostExpect(t, cAlice, srv, "/teams/"+teamID, "/teams/"+teamID+"/invites", url.Values{})
	invBody, _ := io.ReadAll(invResp.Body)
	invResp.Body.Close()

	// Find the plaintext invite URL in the rendered notice.
	urlPat := regexp.MustCompile(`/invites/` + auth.TokenPrefix + `[A-Za-z0-9_-]{43}`)
	m := urlPat.FindString(string(invBody))
	if m == "" {
		// Fall back to looking for the full http(s) URL the page renders.
		urlPat = regexp.MustCompile(`https?://[^"]*?/invites/` + auth.TokenPrefix + `[A-Za-z0-9_-]{43}`)
		full := urlPat.FindString(string(invBody))
		if full == "" {
			t.Fatalf("no invite URL in team page body:\n%s", invBody)
		}
		idx := strings.Index(full, "/invites/")
		m = full[idx:]
	}
	plain := strings.TrimPrefix(m, "/invites/")

	// New user (anon) hits the invite page → sees CTA to /signup?invite=:t.
	cNewbie := clientWithJar(t)
	_, confirmBody := mustGet(t, cNewbie, srv.URL+m)
	if !strings.Contains(confirmBody, "Sign up to join") {
		t.Errorf("invite page should show signup CTA, body:\n%s", confirmBody)
	}

	// Hit /signup?invite=:t — form should carry the hidden invite field.
	_, signupBody := mustGet(t, cNewbie, srv.URL+"/signup?invite="+plain)
	if !strings.Contains(signupBody, `name="invite"`) {
		t.Errorf("signup form should carry hidden invite field, body:\n%s", signupBody)
	}
	csrf := extractCSRFToken(t, signupBody)

	// Sign up with the invite. Should redirect to / (post-signup home).
	form := url.Values{
		"email":      {"newbie@example.com"},
		"password":   {"correct horse battery staple"},
		"invite":     {plain},
		"csrf_token": {csrf},
	}
	signupResp, err := cNewbie.PostForm(srv.URL+"/signup", form)
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	signupResp.Body.Close()
	if signupResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("signup status: %d want 303", signupResp.StatusCode)
	}

	// New user's home should list TWO teams now.
	_, newHome := mustGet(t, cNewbie, srv.URL+"/")
	if strings.Count(newHome, `href="/teams/`) < 2 {
		t.Errorf("post-signup-with-invite home should list 2 teams, body:\n%s", newHome)
	}
}

// TestInviteFlow_StrictOnInvalid (Issue 12): signup with a bogus invite
// fails the form, no user is created.
func TestInviteFlow_StrictOnInvalid(t *testing.T) {
	srv := startStack(t)
	c := clientWithJar(t)

	bogus := auth.TokenPrefix + strings.Repeat("z", 43)
	_, body := mustGet(t, c, srv.URL+"/signup?invite="+bogus)
	csrf := extractCSRFToken(t, body)
	form := url.Values{
		"email":      {"ghost@example.com"},
		"password":   {"correct horse battery staple"},
		"invite":     {bogus},
		"csrf_token": {csrf},
	}
	resp, err := c.PostForm(srv.URL+"/signup", form)
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: %d want 400", resp.StatusCode)
	}
	if !strings.Contains(string(respBody), "no longer valid") {
		t.Errorf("error message missing, body:\n%s", respBody)
	}

	// And no session cookie was set.
	if hasSessionCookie(t, c, srv.URL) {
		t.Errorf("failed signup should not set a session cookie")
	}
}

// TestInvite_CSRFOnPost (Issue 9): the POST consume route is CSRF-protected.
// A POST without the token returns 403, blocking the cross-site auto-join attack.
func TestInvite_CSRFOnPost(t *testing.T) {
	srv := startStack(t)
	cAlice := signupClient(t, srv, "alice@example.com")
	cBob := signupClient(t, srv, "bob@example.com")

	// Alice issues an invite.
	_, aliceHome := mustGet(t, cAlice, srv.URL+"/")
	teamID := findIDFromHref(t, aliceHome, "/teams/")
	invResp := formPostExpect(t, cAlice, srv, "/teams/"+teamID, "/teams/"+teamID+"/invites", url.Values{})
	body, _ := io.ReadAll(invResp.Body)
	invResp.Body.Close()
	urlPat := regexp.MustCompile(`/invites/` + auth.TokenPrefix + `[A-Za-z0-9_-]{43}`)
	invPath := urlPat.FindString(string(body))
	if invPath == "" {
		urlPat = regexp.MustCompile(`https?://[^"]*?/invites/` + auth.TokenPrefix + `[A-Za-z0-9_-]{43}`)
		full := urlPat.FindString(string(body))
		invPath = full[strings.Index(full, "/invites/"):]
	}

	// Bob POSTs without a CSRF token → 403, no membership created.
	resp, err := cBob.PostForm(srv.URL+invPath, url.Values{})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status: %d want 403", resp.StatusCode)
	}
}

// TestMustChangePassword_GateRedirects exercises Issue 4: a user with
// must_change_password=true is redirected to /account/password from
// arbitrary protected routes, but can reach /account/password and /logout.
func TestMustChangePassword_GateRedirects(t *testing.T) {
	srv, pool := startStackWithPool(t)
	c := signupClient(t, srv, "alice@example.com")

	// Flip the flag directly in PG (simulates the operator reset path).
	_, err := pool.Exec(context.Background(), `UPDATE users SET must_change_password = true WHERE email = 'alice@example.com'`)
	if err != nil {
		t.Fatalf("force flag: %v", err)
	}

	// First protected request after flag flips → 303 to /account/password.
	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("flagged GET / status: %d want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/account/password" {
		t.Errorf("redirect: %q want /account/password", got)
	}

	// /account/password is reachable.
	pwResp, err := c.Get(srv.URL + "/account/password")
	if err != nil {
		t.Fatalf("GET /account/password: %v", err)
	}
	pwBody, _ := io.ReadAll(pwResp.Body)
	pwResp.Body.Close()
	if pwResp.StatusCode != http.StatusOK {
		t.Errorf("status: %d want 200", pwResp.StatusCode)
	}
	if !strings.Contains(string(pwBody), "reset by an operator") {
		t.Errorf("forced banner missing, body:\n%s", pwBody)
	}

	// Successful change clears the flag and the gate no longer fires.
	csrf := extractCSRFToken(t, string(pwBody))
	form := url.Values{
		"current_password": {"correct horse battery staple"},
		"new_password":     {"new correct horse battery"},
		"csrf_token":       {csrf},
	}
	changeResp, err := c.PostForm(srv.URL+"/account/password", form)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	changeResp.Body.Close()
	if changeResp.StatusCode != http.StatusSeeOther {
		t.Errorf("change pw status: %d want 303", changeResp.StatusCode)
	}

	// Next request to / no longer gated.
	homeResp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("GET / after change: %v", err)
	}
	homeResp.Body.Close()
	if homeResp.StatusCode != http.StatusOK {
		t.Errorf("post-change GET / status: %d want 200", homeResp.StatusCode)
	}
}

// TestInvalidInvite_ReturnsConfirmPageWith404 covers the unknown-invite UX
// branch: hitting /invites/<garbage> renders the "invite no longer valid"
// page with a 404 status.
func TestInvalidInvite_ReturnsConfirmPageWith404(t *testing.T) {
	srv := startStack(t)
	c := clientWithJar(t)

	resp, err := c.Get(srv.URL + "/invites/" + auth.TokenPrefix + strings.Repeat("z", 43))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status: %d want 404", resp.StatusCode)
	}
	if !strings.Contains(string(body), "no longer valid") {
		t.Errorf("body missing message, got:\n%s", body)
	}
}

// startStackWithPool returns the same stack as startStack but also exposes
// the underlying pgx pool so tests can mutate state directly (e.g. flipping
// must_change_password to simulate the operator reset path).
func startStackWithPool(t *testing.T) (*httptest.Server, *pgxpool.Pool) {
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
	return srv, pool
}
