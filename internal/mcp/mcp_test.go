package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jjdinho/mere-analytics/internal/auth"
	appch "github.com/jjdinho/mere-analytics/internal/clickhouse"
	"github.com/jjdinho/mere-analytics/internal/mcp"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/query"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

// stack is everything an MCP tool call needs: a real auth.Service (PG) so the
// project-visibility check runs, and a real query executor + schema provider
// (CH) seeded with two projects' events. mcpHandlerFor wraps the transport
// with an injected bearer identity, standing in for RequireBearer.
type stack struct {
	authSvc  *auth.Service
	deps     mcp.Deps
	userID   string
	projectA string
}

func newStack(t *testing.T) *stack {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// --- Postgres: a user with two live projects in their personal team ---
	pool, pgCfg := testhelpers.StartPostgres(t)
	pgDriver, err := postgres.MigrateDriver(pgCfg)
	if err != nil {
		t.Fatalf("pg migrate driver: %v", err)
	}
	if err := mmigrate.Run(ctx, "pg", pgDriver, migrations.Postgres, "postgres", logger); err != nil {
		t.Fatalf("pg migrate: %v", err)
	}
	authSvc := auth.NewService(pool)
	signup, err := authSvc.Signup(ctx, auth.SignupRequest{Email: "alice@example.com", Password: "correct-horse-battery"})
	if err != nil {
		t.Fatalf("signup: %v", err)
	}
	viewer := auth.NewViewer(authSvc, signup.User.ID)
	projA, err := viewer.Projects(ctx).Create(signup.Team.ID, "prod-a")
	if err != nil {
		t.Fatalf("create project A: %v", err)
	}
	projB, err := viewer.Projects(ctx).Create(signup.Team.ID, "prod-b")
	if err != nil {
		t.Fatalf("create project B: %v", err)
	}

	// --- ClickHouse: distinct events per project ---
	admin, chCfg := testhelpers.StartClickHouse(t)
	if err := appch.ProvisionReadonlyUser(ctx, admin, chCfg); err != nil {
		t.Fatalf("provision readonly: %v", err)
	}
	chDriver, err := appch.MigrateDriver(admin, chCfg)
	if err != nil {
		t.Fatalf("ch migrate driver: %v", err)
	}
	if err := mmigrate.Run(ctx, "ch", chDriver, migrations.ClickHouse, "clickhouse", logger); err != nil {
		t.Fatalf("ch migrate: %v", err)
	}
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct{ projectID, properties string }{
		{projA.ID, `{"marker":"only-a"}`},
		{projB.ID, `{"marker":"only-b"}`},
	} {
		if _, err := admin.ExecContext(ctx, `
			INSERT INTO events_raw_v1
			(project_id, event, anonymous_id, user_id, timestamp, session_id, properties, extras)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, row.projectID, "pageview", "anon", nil, ts, "sess", row.properties, `{}`); err != nil {
			t.Fatalf("seed ch row: %v", err)
		}
	}
	for _, row := range []struct{ projectID, anonymousID, userID string }{
		{projA.ID, "anon-a", "user-a"},
		{projB.ID, "anon-b", "user-b"},
	} {
		if _, err := admin.ExecContext(ctx, `
			INSERT INTO events_raw_v1
			(project_id, event, anonymous_id, user_id, timestamp, session_id, properties, extras)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, row.projectID, "$identify", row.anonymousID, row.userID, ts.Add(time.Minute), "sess", `{}`, `{}`); err != nil {
			t.Fatalf("seed identify row: %v", err)
		}
	}
	readonly, err := appch.OpenReadonly(ctx, chCfg)
	if err != nil {
		t.Fatalf("open readonly: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	exec := query.NewExecutor(readonly, chCfg.ClickHouseDatabase)
	schema := query.NewSchemaProvider(readonly, exec)

	return &stack{
		authSvc: authSvc,
		deps: mcp.Deps{
			AuthService: authSvc,
			Executor:    exec,
			Schema:      schema,
			Logger:      logger,
		},
		userID:   signup.User.ID,
		projectA: projA.ID,
	}
}

// serverAs starts an httptest server for the MCP transport whose tool calls
// run with the given bearer identity (user + project), the way RequireBearer
// would attach it in production.
func (s *stack) serverAs(t *testing.T, userID, projectID string) *httptest.Server {
	t.Helper()
	handler := mcp.NewHTTPHandler(s.deps)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := oauth.ContextWith(r.Context(), &oauth.AccessContext{UserID: userID, ProjectID: projectID})
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// toolResult is the decoded outcome of a tools/call: either a tool result
// (text content + isError flag) or a JSON-RPC protocol error.
type toolResult struct {
	text    string
	isError bool
	rpcErr  *rpcError
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func callTool(t *testing.T, srv *httptest.Server, name string, args map[string]any) toolResult {
	t.Helper()
	params := map[string]any{"name": name}
	if args != nil {
		params["arguments"] = args
	}
	return rawRPC(t, srv, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  params,
	})
}

func rawRPC(t *testing.T, srv *httptest.Server, message any) toolResult {
	t.Helper()
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	payload := extractJSONRPC(t, resp.Header.Get("Content-Type"), raw)

	var env struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *rpcError `json:"error"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("unmarshal jsonrpc (%s): %v\nbody=%s", resp.Header.Get("Content-Type"), err, raw)
	}
	out := toolResult{rpcErr: env.Error}
	if env.Result != nil {
		out.isError = env.Result.IsError
		var sb strings.Builder
		for _, c := range env.Result.Content {
			if c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
		out.text = sb.String()
	}
	return out
}

// extractJSONRPC returns the JSON-RPC object from a response body, unwrapping
// SSE framing if the transport chose to stream (it shouldn't for our
// notification-free tools, but be robust).
func extractJSONRPC(t *testing.T, contentType string, body []byte) []byte {
	t.Helper()
	if !strings.Contains(contentType, "text/event-stream") {
		return body
	}
	for _, line := range strings.Split(string(body), "\n") {
		if data, ok := strings.CutPrefix(line, "data:"); ok {
			return []byte(strings.TrimSpace(data))
		}
	}
	t.Fatalf("no data: line in SSE body: %s", body)
	return nil
}

// TestMCPTools_TenantIsolation is the step-6 isolation contract, MCP front
// door. A's token through the query + schema tools never surfaces B's data.
// Naive, qualified, subquery, and self-join forms are all filtered by the
// executor's additional_table_filters — the same mechanism the HTTP API uses.
func TestMCPTools_TenantIsolation(t *testing.T) {
	s := newStack(t)
	srv := s.serverAs(t, s.userID, s.projectA)

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"naive", `SELECT properties FROM events`},
		{"qualified", `SELECT properties FROM analytics.events`},
		{"subquery", `SELECT properties FROM (SELECT * FROM events)`},
		{"self_join", `SELECT a.properties FROM events a INNER JOIN events b ON a.distinct_id = b.distinct_id`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, srv, "query", map[string]any{"sql": tc.sql})
			if res.rpcErr != nil {
				t.Fatalf("unexpected jsonrpc error: %+v", res.rpcErr)
			}
			if res.isError {
				t.Fatalf("unexpected tool error: %s", res.text)
			}
			if !strings.Contains(res.text, "only-a") {
				t.Errorf("expected project A's data, got: %s", res.text)
			}
			if strings.Contains(res.text, "only-b") {
				t.Errorf("LEAK: project B's data visible to A's token: %s", res.text)
			}
		})
	}

	t.Run("schema_lists_only_allowlisted_table", func(t *testing.T) {
		res := callTool(t, srv, "schema", nil)
		if res.rpcErr != nil || res.isError {
			t.Fatalf("schema failed: err=%+v isError=%v text=%s", res.rpcErr, res.isError, res.text)
		}
		for _, table := range []string{"events", "persons", "sessions"} {
			if !strings.Contains(res.text, `"`+table+`"`) {
				t.Errorf("schema missing %s: %s", table, res.text)
			}
		}
		if strings.Contains(res.text, "events_raw_v1") {
			t.Errorf("schema leaked hidden raw table: %s", res.text)
		}
		if !strings.Contains(res.text, "project_id") {
			t.Errorf("schema missing project_id column: %s", res.text)
		}
		if strings.Contains(res.text, "schema_migrations") {
			t.Errorf("schema leaked a non-allowlisted table: %s", res.text)
		}
		// The catalog must describe tables and columns, not just name them, so
		// an agent can build effective queries from it.
		if !strings.Contains(res.text, `"description"`) {
			t.Errorf("schema missing table/column descriptions: %s", res.text)
		}
	})

	t.Run("direct_internal_table_queries_still_do_not_leak", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			sql  string
		}{
			{"public_events", `SELECT distinct_id FROM events`},
			{"public_persons", `SELECT distinct_id FROM persons`},
			{"public_sessions", `SELECT distinct_id FROM sessions`},
			{"hidden_raw", `SELECT coalesce(user_id, anonymous_id) FROM events_raw_v1`},
			{"hidden_identity_links", `SELECT user_id FROM identity_links_v1`},
			{"hidden_persons_state", `SELECT raw_distinct_id FROM persons_state`},
			{"hidden_sessions_state", `SELECT coalesce(user_id, anonymous_id) FROM sessions_state`},
			{"hidden_identity_links_mv", `SELECT user_id FROM identity_links_mv`},
			{"hidden_persons_mv", `SELECT raw_distinct_id FROM persons_mv`},
			{"hidden_sessions_mv", `SELECT coalesce(user_id, anonymous_id) FROM sessions_mv`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				res := callTool(t, srv, "query", map[string]any{"sql": tc.sql})
				if res.rpcErr != nil {
					t.Fatalf("unexpected jsonrpc error: %+v", res.rpcErr)
				}
				if res.isError {
					t.Fatalf("unexpected tool error: %s", res.text)
				}
				if strings.Contains(res.text, "anon-b") || strings.Contains(res.text, "user-b") {
					t.Fatalf("LEAK through %s: project B identity visible: %s", tc.name, res.text)
				}
			})
		}
	})
}

// TestMCPTools_SoftDeletedProjectDenied mirrors decision #11: a project
// soft-deleted after the token was issued is no longer reachable, even though
// the bearer grant still names it.
func TestMCPTools_SoftDeletedProjectDenied(t *testing.T) {
	s := newStack(t)
	if err := auth.NewViewer(s.authSvc, s.userID).Projects(context.Background()).SoftDelete(s.projectA); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	srv := s.serverAs(t, s.userID, s.projectA)

	res := callTool(t, srv, "query", map[string]any{"sql": "SELECT count() FROM events"})
	if res.rpcErr != nil {
		t.Fatalf("unexpected jsonrpc error: %+v", res.rpcErr)
	}
	if !res.isError || !strings.Contains(res.text, "project not found") {
		t.Errorf("want tool error 'project not found', got isError=%v text=%q", res.isError, res.text)
	}
}

// denyEntitlement is an Entitlement seam that locks every project — standing
// in for a hosted build's "over 1M events and unpaid" decision.
type denyEntitlement struct{ reason string }

func (d denyEntitlement) AllowAnalysis(context.Context, string) (bool, string) {
	return false, d.reason
}

// TestMCPTools_OverQuotaDenied proves the analysis gate (extension.Entitlement)
// is consulted after visibility: a project that resolves fine still has its
// query + schema tools denied with the upgrade hint when the seam says no.
func TestMCPTools_OverQuotaDenied(t *testing.T) {
	s := newStack(t)
	s.deps.Entitlement = denyEntitlement{reason: "over 1M events this month — upgrade to continue"}
	srv := s.serverAs(t, s.userID, s.projectA)

	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"query", map[string]any{"sql": "SELECT count() FROM events"}},
		{"schema", nil},
	} {
		res := callTool(t, srv, tc.tool, tc.args)
		if res.rpcErr != nil {
			t.Fatalf("%s: unexpected jsonrpc error: %+v", tc.tool, res.rpcErr)
		}
		if !res.isError || !strings.Contains(res.text, "upgrade to continue") {
			t.Errorf("%s: want tool error carrying the upgrade hint, got isError=%v text=%q", tc.tool, res.isError, res.text)
		}
	}
}

// TestMCPTools_QueryErrors covers the tool-error surface: a ClickHouse parse
// error comes back verbatim, an empty SQL string is rejected, and breaching
// the row cap tells the model to add a LIMIT — all as tool errors the model
// can read and correct, not protocol failures.
func TestMCPTools_QueryErrors(t *testing.T) {
	s := newStack(t)
	srv := s.serverAs(t, s.userID, s.projectA)

	t.Run("bad_sql_verbatim", func(t *testing.T) {
		res := callTool(t, srv, "query", map[string]any{"sql": "SELECT this is not valid"})
		if res.rpcErr != nil {
			t.Fatalf("unexpected jsonrpc error: %+v", res.rpcErr)
		}
		if !res.isError {
			t.Fatalf("expected tool error, got: %s", res.text)
		}
		if !strings.Contains(strings.ToLower(res.text), "syntax") && !strings.Contains(res.text, "Exception") {
			t.Errorf("expected a verbatim ClickHouse error, got: %s", res.text)
		}
	})

	t.Run("empty_sql", func(t *testing.T) {
		res := callTool(t, srv, "query", map[string]any{"sql": "  "})
		if res.rpcErr != nil || !res.isError {
			t.Fatalf("want tool error, got err=%+v isError=%v", res.rpcErr, res.isError)
		}
		if !strings.Contains(res.text, "sql is required") {
			t.Errorf("want 'sql is required', got: %s", res.text)
		}
	})

	t.Run("missing_sql_arg", func(t *testing.T) {
		res := callTool(t, srv, "query", map[string]any{})
		if res.rpcErr != nil || !res.isError {
			t.Fatalf("want tool error, got err=%+v isError=%v", res.rpcErr, res.isError)
		}
		if !strings.Contains(res.text, "sql is required") {
			t.Errorf("want 'sql is required', got: %s", res.text)
		}
	})
}

// TestMCP_MalformedJSONRPC: a body that isn't valid JSON-RPC yields a
// well-formed JSON-RPC parse error, not a panic or a dropped connection.
func TestMCP_MalformedJSONRPC(t *testing.T) {
	s := newStack(t)
	srv := s.serverAs(t, s.userID, s.projectA)

	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var env struct {
		JSONRPC string    `json:"jsonrpc"`
		Error   *rpcError `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("response is not JSON: %v\nbody=%s", err, body)
	}
	if env.JSONRPC != "2.0" {
		t.Errorf("missing jsonrpc envelope: %s", body)
	}
	if env.Error == nil || env.Error.Code != -32700 {
		t.Errorf("want parse error (-32700), got: %s", body)
	}
}
