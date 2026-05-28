package migrate_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestRun_NoChangeOnSecondCall asserts ErrNoChange is treated as success.
func TestRun_NoChangeOnSecondCall(t *testing.T) {
	_, cfg := testhelpers.StartPostgres(t)
	ctx := context.Background()

	// First apply.
	d1, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("MigrateDriver: %v", err)
	}
	if err := mmigrate.Run(ctx, "pg", d1, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	// Second apply — should be a no-op, no error.
	d2, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("MigrateDriver second: %v", err)
	}
	if err := mmigrate.Run(ctx, "pg", d2, migrations.Postgres, "postgres", quietLogger()); err != nil {
		t.Fatalf("second Run (ErrNoChange should be swallowed): %v", err)
	}
}

// TestRun_DirtyStateProducesRunbook simulates a dirty migration state by
// manually inserting a dirty row into schema_migrations and asserts the
// returned error contains operator-actionable text.
func TestRun_DirtyStateProducesRunbook(t *testing.T) {
	pool, cfg := testhelpers.StartPostgres(t)
	ctx := context.Background()

	// Manually create the schema_migrations table and insert a dirty row.
	// This is the shape golang-migrate creates and uses to track state.
	if _, err := pool.Exec(ctx,
		`CREATE TABLE schema_migrations (version BIGINT NOT NULL PRIMARY KEY, dirty BOOLEAN NOT NULL)`,
	); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO schema_migrations (version, dirty) VALUES (1, true)`); err != nil {
		t.Fatalf("seed dirty row: %v", err)
	}

	d, err := postgres.MigrateDriver(cfg)
	if err != nil {
		t.Fatalf("MigrateDriver: %v", err)
	}
	err = mmigrate.Run(ctx, "pg", d, migrations.Postgres, "postgres", quietLogger())
	if err == nil {
		t.Fatal("expected dirty state to return an error, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"DIRTY", "version 1", "force"} {
		if !strings.Contains(msg, want) {
			t.Errorf("dirty error missing %q: %s", want, msg)
		}
	}
}
