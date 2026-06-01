package query_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

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
		projectID  string
		distinctID string
		properties string
	}{
		{projectA, "alice", `{"marker":"only-a"}`},
		{projectB, "bob", `{"marker":"only-b"}`},
	} {
		if _, err := admin.ExecContext(ctx, `
			INSERT INTO events_raw_v1
			(project_id, event, distinct_id, timestamp, session_id, properties, extras)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, row.projectID, "pageview", row.distinctID, ts, "sess", row.properties, `{}`); err != nil {
			t.Fatalf("insert seed row: %v", err)
		}
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
		{"naive", `SELECT properties FROM events_raw_v1`},
		{"qualified", `SELECT properties FROM analytics.events_raw_v1`},
		{"subquery", `SELECT properties FROM (SELECT * FROM events_raw_v1)`},
		{"self_join", `SELECT a.properties FROM events_raw_v1 a INNER JOIN events_raw_v1 b ON a.distinct_id = b.distinct_id`},
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

	schema, err := query.NewSchemaProvider(readonly, exec).Schema(ctx)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	if len(schema.Tables) != 1 || schema.Tables[0].Name != "events_raw_v1" {
		t.Fatalf("schema tables = %+v, want only events_raw_v1", schema.Tables)
	}
	foundProjectID := false
	for _, col := range schema.Tables[0].Columns {
		if col.Name == "project_id" {
			foundProjectID = true
		}
		if col.Name == "schema_migrations" {
			t.Fatalf("schema exposed non-allowlisted column/table marker: %+v", schema.Tables)
		}
	}
	if !foundProjectID {
		t.Fatalf("schema missing project_id column: %+v", schema.Tables[0].Columns)
	}
}
