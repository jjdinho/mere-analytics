package web_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/clickhouse"
	"github.com/jjdinho/mere-analytics/internal/config"
	"github.com/jjdinho/mere-analytics/internal/ingest"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/postgres/db"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/internal/web"
	"github.com/jjdinho/mere-analytics/migrations"
)

// ingestStack is the per-test infrastructure: real PG (migrated), real CH
// (migrated), an ingest.Service, the wired web.Handler, and the auth + oauth
// services backing /api/v1/whoami's bearer + the project-creation flow used to
// mint a mere_pub_ token.
type ingestStack struct {
	srv         *httptest.Server
	authSvc     *auth.Service
	oauthSvc    *oauth.Service
	ingestSvc   *ingest.Service
	pgPool      *pgxpool.Pool
	chAdmin     *sql.DB
	pgContainer testcontainers.Container
	chContainer testcontainers.Container
}

// startIngestStack runs the full PG+CH bring-up + migrations once per test,
// then assembles the web handler with ingest wired in. Defaults: kill switch
// off, generous buffers, 50ms flush interval so tests don't burn seconds.
//
// opts mutators let tests tweak the ingest.Options before NewService.
func startIngestStack(t *testing.T, opts ...func(*ingest.Options)) *ingestStack {
	t.Helper()
	pgPool, pgCfg := testhelpers.StartPostgres(t)
	chAdmin, chCfg := testhelpers.StartClickHouse(t)
	return assembleIngestStack(t, pgPool, pgCfg, chAdmin, chCfg, nil, nil, opts...)
}

// startIngestChaosStack is startIngestStack with the raw container handles
// exposed so a test can Stop/Start the actual PG/CH dependencies and assert
// the pipeline heals itself (DLQ drains back into CH; the fatal flag clears).
// It uses the …C helpers; everything downstream is shared with
// startIngestStack.
func startIngestChaosStack(t *testing.T, opts ...func(*ingest.Options)) *ingestStack {
	t.Helper()
	pgPool, pgCfg, pgContainer := testhelpers.StartPostgresC(t)
	chAdmin, chCfg, chContainer := testhelpers.StartClickHouseC(t)
	return assembleIngestStack(t, pgPool, pgCfg, chAdmin, chCfg, pgContainer, chContainer, opts...)
}

// assembleIngestStack runs migrations against the supplied PG+CH handles and
// wires the ingest.Service + web.Handler. Shared by both stack constructors;
// the container args are nil for the non-chaos path.
func assembleIngestStack(
	t *testing.T,
	pgPool *pgxpool.Pool, pgCfg config.Config,
	chAdmin *sql.DB, chCfg config.Config,
	pgContainer, chContainer testcontainers.Container,
	opts ...func(*ingest.Options),
) *ingestStack {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	pgDrv, err := postgres.MigrateDriver(pgCfg)
	if err != nil {
		t.Fatalf("pg migrate driver: %v", err)
	}
	if err := mmigrate.Run(ctx, "pg", pgDrv, migrations.Postgres, "postgres", logger); err != nil {
		t.Fatalf("pg migrate: %v", err)
	}

	chDrv, err := clickhouse.MigrateDriver(chAdmin, chCfg)
	if err != nil {
		t.Fatalf("ch migrate driver: %v", err)
	}
	if err := mmigrate.Run(ctx, "ch", chDrv, migrations.ClickHouse, "clickhouse", logger); err != nil {
		t.Fatalf("ch migrate: %v", err)
	}

	options := ingest.Options{
		EventBuffer:          50_000,
		FlushEvents:          5_000,
		FlushInterval:        50 * time.Millisecond,
		ShutdownGrace:        2 * time.Second,
		Disabled:             false,
		MaxBodyBytes:         10 * 1024 * 1024,
		DLQDrainBatchLimit:   10,
		DLQDepth503Threshold: 100_000,
	}
	for _, opt := range opts {
		opt(&options)
	}
	authSvc := auth.NewService(pgPool)
	oauthSvc := oauth.NewService(pgPool)
	ingestSvc := ingest.NewService(pgPool, chAdmin, options, logger)
	ingestSvc.Start(ctx)
	t.Cleanup(func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = ingestSvc.Shutdown(shutCtx)
	})

	srv := httptest.NewServer(web.Handler(web.Options{
		AuthService:          authSvc,
		OAuthService:         oauthSvc,
		OAuthIssuer:          "https://issuer.test",
		Logger:               logger,
		SecureCookies:        false,
		IngestService:        ingestSvc,
		AllowedOrigins:       nil,
		IngestMaxBodyBytes:   options.MaxBodyBytes,
		DLQDepth503Threshold: options.DLQDepth503Threshold,
	}))
	t.Cleanup(srv.Close)

	return &ingestStack{
		srv:         srv,
		authSvc:     authSvc,
		oauthSvc:    oauthSvc,
		ingestSvc:   ingestSvc,
		pgPool:      pgPool,
		chAdmin:     chAdmin,
		pgContainer: pgContainer,
		chContainer: chContainer,
	}
}

// mintProjectAndToken creates a fresh user + project via the real web flow
// and returns the bootstrap mere_pub_ token and its project ID. Reuses the
// existing signupClient + project-create helpers so we exercise the auto-
// provisioned token path end-to-end.
func mintProjectAndToken(t *testing.T, st *ingestStack) (token, projectID string) {
	t.Helper()
	const pw = "correct horse battery staple"
	email := fmt.Sprintf("ingest-%d@example.com", time.Now().UnixNano())
	if _, err := st.authSvc.Signup(context.Background(), auth.SignupRequest{Email: email, Password: pw}); err != nil {
		t.Fatalf("signup: %v", err)
	}
	c := loginAs(t, st.srv, email, pw)

	_, home := mustGet(t, c, st.srv.URL+"/")
	teamID := findIDFromHref(t, home, "/teams/")
	resp := formPostExpect(t, c, st.srv, "/", "/teams/"+teamID+"/projects",
		map[string][]string{"name": {"prod"}})
	projectURL := resp.Header.Get("Location")
	resp.Body.Close()
	projectID = strings.TrimPrefix(projectURL, "/projects/")

	// Read the token plaintext from PG — the project page renders it,
	// but reading it directly skips a fragile HTML scrape.
	q := db.New(st.pgPool)
	row, err := q.GetPublicTokenForProjectForUser(context.Background(), db.GetPublicTokenForProjectForUserParams{
		ProjectID: projectID,
		UserID:    userIDByEmail(t, st.pgPool, email),
	})
	if err != nil {
		t.Fatalf("get public token: %v", err)
	}
	return row.TokenPlaintext, projectID
}

func userIDByEmail(t *testing.T, pool *pgxpool.Pool, email string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("user lookup: %v", err)
	}
	return id
}

func postIngestBatch(t *testing.T, url, token string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(body); err != nil {
		t.Fatalf("encode: %v", err)
	}
	req, _ := http.NewRequest("POST", url+"/api/v1/ingest/events", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

// waitForCHCount polls events_raw_v1 until the row count for projectID
// reaches want or deadline passes. Returns the final count.
func waitForCHCount(t *testing.T, ch *sql.DB, projectID string, want int, deadline time.Duration) int {
	t.Helper()
	end := time.Now().Add(deadline)
	var count int
	for time.Now().Before(end) {
		if err := ch.QueryRow(`SELECT count() FROM events_raw_v1 WHERE project_id = ?`, projectID).Scan(&count); err != nil {
			t.Fatalf("ch count: %v", err)
		}
		if count >= want {
			return count
		}
		time.Sleep(20 * time.Millisecond)
	}
	return count
}

// TestIngest_HappyPath exercises the full pipeline: project creation auto-
// provisions a mere_pub_ token, the handler accepts a 100-event batch with
// 202, the flusher batches to ClickHouse, and a CH read confirms the rows
// land scoped to the project.
func TestIngest_HappyPath(t *testing.T) {
	st := startIngestStack(t)
	token, projectID := mintProjectAndToken(t, st)

	events := make([]map[string]any, 100)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range events {
		events[i] = map[string]any{
			"event":     "pageview",
			"timestamp": now,
		}
	}
	resp := postIngestBatch(t, st.srv.URL, token, map[string]any{"events": events})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status: %d want 202; body=%s", resp.StatusCode, body)
	}
	var parsed struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Accepted != 100 {
		t.Errorf("accepted: %d want 100", parsed.Accepted)
	}

	if got := waitForCHCount(t, st.chAdmin, projectID, 100, 5*time.Second); got != 100 {
		t.Errorf("ch count: %d want 100", got)
	}
}

// TestIngest_LenientExtrasAndEpochTimestamp exercises the plan's "lenient on
// extras" + "epoch ms timestamp" contract end-to-end: an event carrying a
// stray top-level field and a numeric (epoch-millis) timestamp is accepted
// (not rejected), and the stray field lands verbatim in the extras column with
// the timestamp resolved to the correct instant.
func TestIngest_LenientExtrasAndEpochTimestamp(t *testing.T) {
	st := startIngestStack(t)
	token, projectID := mintProjectAndToken(t, st)

	const epochMs = int64(1717200000000)
	body := map[string]any{"events": []map[string]any{
		{"event": "purchase", "timestamp": epochMs, "plan_tier": "pro"},
	}}
	resp := postIngestBatch(t, st.srv.URL, token, body)
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status: %d want 202 (stray field must not be rejected); body=%s", resp.StatusCode, raw)
	}
	var parsed struct {
		Accepted int `json:"accepted"`
		Rejected int `json:"rejected"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if parsed.Accepted != 1 || parsed.Rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d want 1/0", parsed.Accepted, parsed.Rejected)
	}

	if got := waitForCHCount(t, st.chAdmin, projectID, 1, 5*time.Second); got != 1 {
		t.Fatalf("ch count: %d want 1", got)
	}

	var extras string
	var ts time.Time
	if err := st.chAdmin.QueryRow(
		`SELECT extras, timestamp FROM events_raw_v1 WHERE project_id = ? LIMIT 1`, projectID,
	).Scan(&extras, &ts); err != nil {
		t.Fatalf("ch read: %v", err)
	}
	var extrasMap map[string]any
	if err := json.Unmarshal([]byte(extras), &extrasMap); err != nil {
		t.Fatalf("extras not valid JSON (%q): %v", extras, err)
	}
	if extrasMap["plan_tier"] != "pro" {
		t.Errorf("extras: got %v want plan_tier=pro", extrasMap)
	}
	if want := time.UnixMilli(epochMs).UTC(); !ts.Equal(want) {
		t.Errorf("timestamp: got %s want %s", ts.UTC(), want)
	}
}

// TestIngest_AuthEdges covers the four 401 surfaces + the 500 surface (the
// last requires PG-down, which we simulate by closing the pool — see
// TestIngest_PGDownReturns500).
func TestIngest_AuthEdges(t *testing.T) {
	st := startIngestStack(t)
	goodToken, projectID := mintProjectAndToken(t, st)
	_ = projectID

	now := time.Now().UTC().Format(time.RFC3339Nano)
	body := map[string]any{"events": []map[string]any{{"event": "x", "timestamp": now}}}
	encode := func() *bytes.Buffer {
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(body)
		return &buf
	}
	post := func(headers map[string]string) *http.Response {
		req, _ := http.NewRequest("POST", st.srv.URL+"/api/v1/ingest/events", encode())
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		return resp
	}

	cases := []struct {
		name       string
		auth       string
		wantStatus int
	}{
		{"missing authorization", "", http.StatusUnauthorized},
		{"non-bearer scheme", "Basic foo", http.StatusUnauthorized},
		{"non-prefix bearer (oauth-style)", "Bearer some-43char-opaque-token-without-mere-pub", http.StatusUnauthorized},
		{"unknown mere_pub token", "Bearer mere_pub_unknown-token-value", http.StatusUnauthorized},
		{"valid token + good batch", "Bearer " + goodToken, http.StatusAccepted},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			headers := map[string]string{}
			if c.auth != "" {
				headers["Authorization"] = c.auth
			}
			resp := post(headers)
			resp.Body.Close()
			if resp.StatusCode != c.wantStatus {
				t.Errorf("status: %d want %d", resp.StatusCode, c.wantStatus)
			}
		})
	}
}

// TestIngest_SoftDeletedProject401 sets projects.deleted_at and verifies a
// previously-valid token no longer authorizes.
func TestIngest_SoftDeletedProject401(t *testing.T) {
	st := startIngestStack(t)
	token, projectID := mintProjectAndToken(t, st)

	if _, err := st.pgPool.Exec(context.Background(),
		`UPDATE projects SET deleted_at = NOW() WHERE id = $1`, projectID); err != nil {
		t.Fatalf("soft-delete: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	resp := postIngestBatch(t, st.srv.URL, token, map[string]any{
		"events": []map[string]any{{"event": "x", "timestamp": now}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status: %d want 401", resp.StatusCode)
	}
}

// TestIngest_PGDownReturns500 closes the pgPool so LookupIngestToken returns
// an infrastructure error; the middleware must surface that as 500, never
// 401 — conflating would make a PG outage look like a credential-stuffing
// attack signal.
func TestIngest_PGDownReturns500(t *testing.T) {
	st := startIngestStack(t)
	token, _ := mintProjectAndToken(t, st)

	st.pgPool.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	resp := postIngestBatch(t, st.srv.URL, token, map[string]any{
		"events": []map[string]any{{"event": "x", "timestamp": now}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status: %d want 500", resp.StatusCode)
	}
}

// TestIngest_OversizedBody asserts MaxBody + DecodeJSON respond with 413
// when the request body overruns the configured ceiling. The body must be
// valid JSON whose token stream is still being read when the cap fires —
// random bytes would just trip the syntax-error → 400 branch.
func TestIngest_OversizedBody(t *testing.T) {
	st := startIngestStack(t, func(o *ingest.Options) {
		o.MaxBodyBytes = 1024 // 1 KiB ceiling
	})
	token, _ := mintProjectAndToken(t, st)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	// One event whose extras blob is a giant JSON-quoted string. Well over
	// 1 KiB after serialization, so MaxBytesReader fires before Decode
	// returns.
	padding := strings.Repeat("a", 4096)
	body := map[string]any{
		"events": []map[string]any{
			{"event": "x", "timestamp": now, "extras": map[string]any{"pad": padding}},
		},
	}
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(body)
	req, _ := http.NewRequest("POST", st.srv.URL+"/api/v1/ingest/events", &buf)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status: %d want 413", resp.StatusCode)
	}
}

// TestIngest_InvalidJSON asserts a syntactically broken body → 400.
func TestIngest_InvalidJSON(t *testing.T) {
	st := startIngestStack(t)
	token, _ := mintProjectAndToken(t, st)

	req, _ := http.NewRequest("POST", st.srv.URL+"/api/v1/ingest/events", bytes.NewReader([]byte(`{not json}`)))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status: %d want 400", resp.StatusCode)
	}
}

// TestIngest_EmptyBatch asserts an empty events array short-circuits with
// 200 (not 400). The decision sits in postIngest where len(valid)==0 →
// StatusOK + accepted:0; the channel is never touched.
func TestIngest_EmptyBatch(t *testing.T) {
	st := startIngestStack(t)
	token, _ := mintProjectAndToken(t, st)

	resp := postIngestBatch(t, st.srv.URL, token, map[string]any{"events": []map[string]any{}})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status: %d want 200; body=%s", resp.StatusCode, body)
	}
	var parsed struct {
		Accepted int `json:"accepted"`
	}
	_ = json.Unmarshal(body, &parsed)
	if parsed.Accepted != 0 {
		t.Errorf("accepted: %d want 0", parsed.Accepted)
	}
}

// TestIngest_KillSwitch boots the service with INGEST_DISABLED=true. POST
// must 503 immediately without enqueueing.
func TestIngest_KillSwitch(t *testing.T) {
	st := startIngestStack(t, func(o *ingest.Options) {
		o.Disabled = true
	})
	token, _ := mintProjectAndToken(t, st)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	resp := postIngestBatch(t, st.srv.URL, token, map[string]any{
		"events": []map[string]any{{"event": "x", "timestamp": now}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: %d want 503", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Errorf("Retry-After header missing")
	}
}

// TestIngest_CORS covers the three CORS shapes: preflight with permissive
// default, POST response carrying CORS headers, and the restricted-allow-
// list omit-on-mismatch path.
func TestIngest_CORS(t *testing.T) {
	t.Run("permissive default echoes *", func(t *testing.T) {
		st := startIngestStack(t)
		req, _ := http.NewRequest("OPTIONS", st.srv.URL+"/api/v1/ingest/events", nil)
		req.Header.Set("Origin", "https://example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("OPTIONS: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("status: %d want 204", resp.StatusCode)
		}
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Allow-Origin: %q want *", got)
		}
		if got := resp.Header.Get("Access-Control-Allow-Methods"); got == "" {
			t.Errorf("Allow-Methods missing")
		}
	})

	t.Run("POST carries CORS headers", func(t *testing.T) {
		st := startIngestStack(t)
		token, _ := mintProjectAndToken(t, st)
		now := time.Now().UTC().Format(time.RFC3339Nano)
		req, _ := http.NewRequest("POST", st.srv.URL+"/api/v1/ingest/events",
			bytes.NewBufferString(fmt.Sprintf(`{"events":[{"event":"x","timestamp":%q}]}`, now)))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://example.com")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("Allow-Origin: %q want *", got)
		}
	})

	t.Run("restricted allow list omits mismatched origin", func(t *testing.T) {
		// Direct middleware exercise — no DB needed.
		var allowed = "https://allowed.example"
		var disallowed = "https://other.example"
		cors := web.CORS([]string{allowed})
		downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		h := cors(downstream)

		// Allowed origin echoes.
		rec := httptest.NewRecorder()
		req, _ := http.NewRequest("OPTIONS", "/api/v1/ingest/events", nil)
		req.Header.Set("Origin", allowed)
		req.Header.Set("Access-Control-Request-Method", "POST")
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Errorf("allowed status: %d want 204", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != allowed {
			t.Errorf("allowed echo: %q want %q", got, allowed)
		}

		// Disallowed origin: response carries no CORS header.
		rec = httptest.NewRecorder()
		req, _ = http.NewRequest("OPTIONS", "/api/v1/ingest/events", nil)
		req.Header.Set("Origin", disallowed)
		req.Header.Set("Access-Control-Request-Method", "POST")
		h.ServeHTTP(rec, req)
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("disallowed leak: got %q want empty", got)
		}
	})
}

// TestIngest_Saturation519 forces the channel-full path: build a Service
// with a tiny EventBuffer and no flusher (Start not called), then POST
// twice — the second submit must 503 + Retry-After: 1.
func TestIngest_Saturation503(t *testing.T) {
	st := startIngestStack(t, func(o *ingest.Options) {
		o.EventBuffer = 5
		o.FlushEvents = 5
		// keep the flush interval long so the flusher can't drain between
		// the two posts
		o.FlushInterval = 10 * time.Second
	})
	token, _ := mintProjectAndToken(t, st)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	make5 := func() any {
		evs := make([]map[string]any, 5)
		for i := range evs {
			evs[i] = map[string]any{"event": "x", "timestamp": now}
		}
		return map[string]any{"events": evs}
	}

	// First batch fills the ceiling exactly — should land 202.
	resp1 := postIngestBatch(t, st.srv.URL, token, make5())
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusAccepted {
		t.Fatalf("first POST: %d want 202", resp1.StatusCode)
	}
	// Second batch overflows → 503 + Retry-After: 1.
	resp2 := postIngestBatch(t, st.srv.URL, token, make5())
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second POST: %d want 503", resp2.StatusCode)
	}
	if ra := resp2.Header.Get("Retry-After"); ra != "1" {
		t.Errorf("Retry-After: %q want 1", ra)
	}
}

// TestIngest_LastUsedAtStamped exercises the OAuth middleware's fire-and-
// forget last_used_at update. The bearer is obtained through the OAuth
// authorize+token dance; the stamp lands within 60s of the first /api/v1/whoami
// hit; the 60s throttle leaves the second hit untouched.
func TestIngest_LastUsedAtStamped(t *testing.T) {
	st := startIngestStack(t)
	const pw = "correct horse battery staple"
	email := fmt.Sprintf("oauth-%d@example.com", time.Now().UnixNano())
	if _, err := st.authSvc.Signup(context.Background(), auth.SignupRequest{Email: email, Password: pw}); err != nil {
		t.Fatalf("signup: %v", err)
	}
	c := loginAs(t, st.srv, email, pw)

	_, home := mustGet(t, c, st.srv.URL+"/")
	teamID := findIDFromHref(t, home, "/teams/")
	resp := formPostExpect(t, c, st.srv, "/", "/teams/"+teamID+"/projects",
		map[string][]string{"name": {"prod"}})
	projectID := strings.TrimPrefix(resp.Header.Get("Location"), "/projects/")
	resp.Body.Close()

	// Issue an access token directly via the service (cheaper than walking
	// the full authorize/token flow for this column-level assertion).
	userID := userIDByEmail(t, st.pgPool, email)
	clientID := registerOAuthClient(t, st.srv, "http://localhost:9999/cb")
	plaintext, _, err := st.oauthSvc.IssueAccessToken(context.Background(), oauth.IssueAccessTokenParams{
		ClientID:  clientID,
		UserID:    userID,
		ProjectID: projectID,
		Scope:     oauth.ScopeAPI,
	})
	if err != nil {
		t.Fatalf("issue access token: %v", err)
	}

	// First whoami → middleware stamps last_used_at fire-and-forget.
	req, _ := http.NewRequest("GET", st.srv.URL+"/api/v1/whoami", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	if r, err := http.DefaultClient.Do(req); err == nil {
		r.Body.Close()
	}

	// Poll PG until last_used_at goes non-null.
	deadline := time.Now().Add(3 * time.Second)
	var stamp time.Time
	for time.Now().Before(deadline) {
		var t1 sql.NullTime
		if err := st.pgPool.QueryRow(context.Background(),
			`SELECT last_used_at FROM oauth_access_tokens WHERE user_id = $1`, userID).Scan(&t1); err == nil && t1.Valid {
			stamp = t1.Time
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stamp.IsZero() {
		t.Fatal("last_used_at never stamped")
	}

	// Second whoami immediately — 60s throttle predicate must keep the
	// existing stamp.
	req2, _ := http.NewRequest("GET", st.srv.URL+"/api/v1/whoami", nil)
	req2.Header.Set("Authorization", "Bearer "+plaintext)
	if r, err := http.DefaultClient.Do(req2); err == nil {
		r.Body.Close()
	}
	// Give the fire-and-forget goroutine a moment to run.
	time.Sleep(200 * time.Millisecond)
	var t2 sql.NullTime
	if err := st.pgPool.QueryRow(context.Background(),
		`SELECT last_used_at FROM oauth_access_tokens WHERE user_id = $1`, userID).Scan(&t2); err != nil {
		t.Fatalf("second select: %v", err)
	}
	if !t2.Valid || !t2.Time.Equal(stamp) {
		t.Errorf("throttle violated: first=%v second=%v", stamp, t2.Time)
	}

	// Force the row to look 61s stale, then a third hit must advance it.
	if _, err := st.pgPool.Exec(context.Background(),
		`UPDATE oauth_access_tokens SET last_used_at = NOW() - INTERVAL '61 seconds' WHERE user_id = $1`, userID); err != nil {
		t.Fatalf("rewind: %v", err)
	}
	req3, _ := http.NewRequest("GET", st.srv.URL+"/api/v1/whoami", nil)
	req3.Header.Set("Authorization", "Bearer "+plaintext)
	if r, err := http.DefaultClient.Do(req3); err == nil {
		r.Body.Close()
	}
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var t3 sql.NullTime
		if err := st.pgPool.QueryRow(context.Background(),
			`SELECT last_used_at FROM oauth_access_tokens WHERE user_id = $1`, userID).Scan(&t3); err == nil && t3.Valid {
			if time.Since(t3.Time) < 5*time.Second {
				return // advanced past the rewound stamp; done.
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("third hit did not advance last_used_at past the 61s rewind")
}

// TestIngest_DLQOnCHFailure swaps in a CH handle pointed at a dead address,
// posts a batch, and verifies a failed_events row materializes within one
// flush interval. The DLQ path is the entire CH-outage survival story.
func TestIngest_DLQOnCHFailure(t *testing.T) {
	st := startIngestStack(t)
	token, _ := mintProjectAndToken(t, st)

	// Close the admin CH handle so the next flusher INSERT fails. The
	// flusher then writes to failed_events (PG is still alive).
	st.chAdmin.Close()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	resp := postIngestBatch(t, st.srv.URL, token, map[string]any{
		"events": []map[string]any{{"event": "page", "timestamp": now}},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST: %d want 202", resp.StatusCode)
	}

	deadline := time.Now().Add(3 * time.Second)
	var count int64
	for time.Now().Before(deadline) {
		if err := st.pgPool.QueryRow(context.Background(),
			`SELECT COUNT(*) FROM failed_events WHERE quarantined_at IS NULL`).Scan(&count); err != nil {
			t.Fatalf("count: %v", err)
		}
		if count > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("no failed_events row appeared; count=%d", count)
}

// TestHealthz_ReportsDownOnFatal flips the fatal flag and asserts /healthz
// returns 503 with status:"down".
func TestHealthz_ReportsDownOnFatal(t *testing.T) {
	st := startIngestStack(t)
	st.ingestSvc.Flags().SetFatal(true)
	t.Cleanup(func() { st.ingestSvc.Flags().SetFatal(false) })

	resp, err := http.Get(st.srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status: %d want 503; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"status":"down"`) {
		t.Errorf("body missing down status: %s", body)
	}
}

// silence unused warning when narrowing down tests during iteration
var _ = config.Config{}
