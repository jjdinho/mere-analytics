package web

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
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/query"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

func TestAPIProjectQuery_DirectTableQueriesDoNotLeakProjects(t *testing.T) {
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

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
	projectA, err := viewer.Projects(ctx).Create(signup.Team.ID, "prod-a")
	if err != nil {
		t.Fatalf("create project A: %v", err)
	}
	projectB, err := viewer.Projects(ctx).Create(signup.Team.ID, "prod-b")
	if err != nil {
		t.Fatalf("create project B: %v", err)
	}

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
	for _, row := range []struct {
		projectID   string
		anonymousID string
		userID      string
	}{
		{projectA.ID, "anon-a", "user-a"},
		{projectB.ID, "anon-b", "user-b"},
	} {
		if _, err := admin.ExecContext(ctx, `
			INSERT INTO events_raw_v1
			(project_id, event, anonymous_id, user_id, timestamp, session_id, properties, extras)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, row.projectID, "pageview", row.anonymousID, nil, ts, "sess-"+row.anonymousID, `{"marker":"`+row.anonymousID+`"}`, `{}`); err != nil {
			t.Fatalf("seed pageview: %v", err)
		}
		if _, err := admin.ExecContext(ctx, `
			INSERT INTO events_raw_v1
			(project_id, event, anonymous_id, user_id, timestamp, session_id, properties, extras)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, row.projectID, "$identify", row.anonymousID, row.userID, ts.Add(time.Minute), "sess-"+row.anonymousID, `{}`, `{}`); err != nil {
			t.Fatalf("seed identify: %v", err)
		}
	}

	readonly, err := appch.OpenReadonly(ctx, chCfg)
	if err != nil {
		t.Fatalf("open readonly: %v", err)
	}
	t.Cleanup(func() { _ = readonly.Close() })
	exec := query.NewExecutor(readonly, chCfg.ClickHouseDatabase)
	handler := postAPIProjectQuery(authSvc, exec, logger)

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
			bodyBytes, err := json.Marshal(queryRequest{SQL: tc.sql})
			if err != nil {
				t.Fatalf("marshal query: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+projectA.ID+"/query", bytes.NewReader(bodyBytes))
			req.Header.Set("Content-Type", "application/json")
			req.SetPathValue("project_id", projectA.ID)
			req = req.WithContext(oauth.ContextWith(req.Context(), &oauth.AccessContext{
				UserID:    signup.User.ID,
				ProjectID: projectA.ID,
			}))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if strings.Contains(body, "anon-b") || strings.Contains(body, "user-b") {
				t.Fatalf("LEAK through %s: project B identity visible: %s", tc.name, body)
			}
		})
	}
}
