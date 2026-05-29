package postgres_test

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestOpen_AndRunMigrations_Idempotent(t *testing.T) {
	pool, cfg := testhelpers.StartPostgres(t)
	ctx := context.Background()

	// postgres.Open works against the running container.
	pool2, err := postgres.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("postgres.Open: %v", err)
	}
	t.Cleanup(pool2.Close)

	// First migrate run applies schema.
	driver, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("MigrateDriver (first run): %v", err)
	}
	if err := mmigrate.Run(ctx, "pg", driver, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("migrate first run: %v", err)
	}

	// Second run must be idempotent (ErrNoChange swallowed).
	driver2, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("MigrateDriver (second run): %v", err)
	}
	if err := mmigrate.Run(ctx, "pg", driver2, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("migrate second run (must be idempotent): %v", err)
	}

	// All 7 application tables exist (step 4 added team_invites).
	wantTables := []string{"api_tokens", "projects", "sessions", "team_invites", "team_memberships", "teams", "users"}
	gotTables := listTables(t, pool)
	if !equalStrSlices(gotTables, wantTables) {
		t.Errorf("tables: got %v want %v", gotTables, wantTables)
	}

	// Hot-path indexes that step 3+ depends on.
	wantIndexes := []string{
		"users_email_lower_idx",
		"api_tokens_token_hash_active_idx",
		"api_tokens_project_id_idx",
		"projects_team_id_idx",
		"sessions_user_id_idx",
		"team_memberships_user_id_idx",
		"team_invites_token_hash_active_idx",
		"team_invites_team_id_idx",
	}
	for _, idx := range wantIndexes {
		if !indexExists(t, pool, idx) {
			t.Errorf("missing index: %s", idx)
		}
	}
}

func listTables(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT table_name FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name NOT LIKE 'schema_%'`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func indexExists(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexname=$1`, name).Scan(&n)
	if err != nil {
		t.Fatalf("query index %s: %v", name, err)
	}
	return n == 1
}

func equalStrSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
