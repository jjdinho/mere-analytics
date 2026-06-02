package query_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	chdriver "github.com/ClickHouse/clickhouse-go/v2"

	appch "github.com/jjdinho/mere-analytics/internal/clickhouse"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/query"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

func TestExecutorTenantIsolation(t *testing.T) {
	ctx := context.Background()
	admin, cfg := testhelpers.StartClickHouse(t)
	if err := appch.ProvisionReadonlyUser(ctx, admin, cfg); err != nil {
		t.Fatalf("provision readonly: %v", err)
	}
	driver, err := appch.MigrateDriver(admin, cfg)
	if err != nil {
		t.Fatalf("migrate driver: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := mmigrate.Run(ctx, "ch", driver, migrations.ClickHouse, "clickhouse", logger); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	projectA := "00000000-0000-0000-0000-0000000000aa"
	projectB := "00000000-0000-0000-0000-0000000000bb"
	ts := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		projectID   string
		anonymousID string
		userID      *string
		sessionID   string
		properties  string
	}{
		{projectA, "anon-a", nil, "sess-a", `{"marker":"only-a"}`},
		{projectB, "anon-b", nil, "sess-b", `{"marker":"only-b"}`},
	} {
		if _, err := admin.ExecContext(ctx, `
			INSERT INTO events_raw_v1
			(project_id, event, anonymous_id, user_id, timestamp, session_id, properties, extras)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, row.projectID, "pageview", row.anonymousID, row.userID, ts, row.sessionID, row.properties, `{}`); err != nil {
			t.Fatalf("insert seed row: %v", err)
		}
	}
	if _, err := admin.ExecContext(ctx, `
		INSERT INTO events_raw_v1
		(project_id, event, anonymous_id, user_id, timestamp, session_id, properties, extras)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, projectA, "$identify", "anon-a", "user-a", ts.Add(time.Minute), "sess-a", `{}`, `{}`); err != nil {
		t.Fatalf("insert identify row: %v", err)
	}
	if _, err := admin.ExecContext(ctx, `
		INSERT INTO events_raw_v1
		(project_id, event, anonymous_id, user_id, timestamp, session_id, properties, extras)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, projectB, "$identify", "anon-b", "user-b", ts.Add(time.Minute), "sess-b", `{}`, `{}`); err != nil {
		t.Fatalf("insert intruder identify row: %v", err)
	}

	readonly, err := appch.OpenReadonly(ctx, cfg)
	if err != nil {
		t.Fatalf("open readonly: %v", err)
	}
	defer readonly.Close()
	exec := query.NewExecutor(readonly, cfg.ClickHouseDatabase)

	cases := []struct {
		name string
		sql  string
	}{
		{"public_events", `SELECT properties FROM events WHERE event = 'pageview'`},
		{"qualified_public_events", `SELECT properties FROM analytics.events WHERE event = 'pageview'`},
		{"subquery", `SELECT properties FROM (SELECT * FROM events WHERE event = 'pageview')`},
		{"self_join", `SELECT a.properties FROM events a INNER JOIN events b ON a.distinct_id = b.distinct_id WHERE a.event = 'pageview' AND b.event = 'pageview'`},
		{"hidden_raw_still_isolated", `SELECT properties FROM events_raw_v1 WHERE event = 'pageview'`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := exec.Collect(ctx, projectA, tc.sql, 10)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			if len(res.Rows) != 1 {
				t.Fatalf("rows: got %d want 1 (%+v)", len(res.Rows), res.Rows)
			}
			got, _ := res.Rows[0][0].(string)
			if got != `{"marker":"only-a"}` {
				t.Fatalf("visible row = %q, want only project A", got)
			}
		})
	}

	t.Run("events_resolves_pre_identify_anonymous_events", func(t *testing.T) {
		res, err := exec.Collect(ctx, projectA, `
			SELECT event, distinct_id
			FROM events
			WHERE event IN ('pageview', '$identify')
			ORDER BY timestamp
		`, 10)
		if err != nil {
			t.Fatalf("query: %v", err)
		}
		if len(res.Rows) != 2 {
			t.Fatalf("rows: got %d want 2 (%+v)", len(res.Rows), res.Rows)
		}
		for _, row := range res.Rows {
			if row[1] != "user-a" {
				t.Fatalf("distinct_id = %v, want user-a in rows %+v", row[1], res.Rows)
			}
		}
	})

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"persons", `SELECT distinct_id, event_count FROM persons`},
		{"sessions", `SELECT distinct_id, event_count FROM sessions`},
	} {
		t.Run(tc.name+"_materialized_state_is_isolated", func(t *testing.T) {
			res, err := exec.Collect(ctx, projectA, tc.sql, 10)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			for _, row := range res.Rows {
				if row[0] == "anon-b" {
					t.Fatalf("LEAK: project B row visible through %s: %+v", tc.name, res.Rows)
				}
			}
		})
	}

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
		t.Run(tc.name+"_direct_query_never_returns_other_project_identity", func(t *testing.T) {
			res, err := exec.Collect(ctx, projectA, tc.sql, 20)
			if err != nil {
				t.Fatalf("query: %v", err)
			}
			for _, row := range res.Rows {
				got, _ := row[0].(string)
				switch got {
				case "anon-b", "user-b":
					t.Fatalf("LEAK through %s: project B identity %q visible in %+v", tc.name, got, res.Rows)
				}
			}
		})
	}

	t.Run("control_wrong_filter_anchor_leaks_materialized_state", func(t *testing.T) {
		wrongCtx := chdriver.Context(ctx, chdriver.WithSettings(chdriver.Settings{
			"additional_table_filters": "{'analytics.events_raw_v1': 'project_id = ''" + projectA + "'''}",
		}))
		rows, err := readonly.QueryContext(wrongCtx, `SELECT distinct_id FROM persons ORDER BY distinct_id`)
		if err != nil {
			t.Fatalf("control query: %v", err)
		}
		defer rows.Close()
		var got []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatalf("scan: %v", err)
			}
			got = append(got, id)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		leaked := false
		for _, id := range got {
			if id == "anon-b" || id == "user-b" {
				leaked = true
			}
		}
		if !leaked {
			t.Fatalf("control did not demonstrate leak with wrong filter anchor; got %+v", got)
		}
	})

	schema, err := query.NewSchemaProvider(readonly, exec).Schema(ctx)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	if len(schema.Tables) != 3 {
		t.Fatalf("schema tables = %+v, want events/persons/sessions", schema.Tables)
	}
	wantTables := map[string]bool{"events": false, "persons": false, "sessions": false}
	for _, table := range schema.Tables {
		if _, ok := wantTables[table.Name]; ok {
			wantTables[table.Name] = true
		}
		if table.Name == "events_raw_v1" || table.Name == "identity_links_v1" || table.Name == "persons_state" || table.Name == "sessions_state" {
			t.Fatalf("schema exposed hidden table: %+v", schema.Tables)
		}
		for _, col := range table.Columns {
			if col.Name == "schema_migrations" {
				t.Fatalf("schema exposed non-allowlisted column/table marker: %+v", schema.Tables)
			}
		}
	}
	for name, found := range wantTables {
		if !found {
			t.Fatalf("schema missing %s: %+v", name, schema.Tables)
		}
	}
}
