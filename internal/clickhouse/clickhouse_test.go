package clickhouse_test

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jjdinho/mere-analytics/internal/clickhouse"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestBootstrap_AndMigrations_Idempotent(t *testing.T) {
	admin, cfg := testhelpers.StartClickHouse(t)
	ctx := context.Background()

	// CreateDatabase against a container that already has the DB is a no-op
	// (IF NOT EXISTS) — but it MUST succeed.
	if err := clickhouse.CreateDatabase(ctx, cfg); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if err := clickhouse.CreateDatabase(ctx, cfg); err != nil {
		t.Fatalf("CreateDatabase idempotent: %v", err)
	}

	// Provision readonly user; run twice to assert idempotency (ALTER + GRANT).
	if err := clickhouse.ProvisionReadonlyUser(ctx, admin, cfg); err != nil {
		t.Fatalf("ProvisionReadonlyUser: %v", err)
	}
	if err := clickhouse.ProvisionReadonlyUser(ctx, admin, cfg); err != nil {
		t.Fatalf("ProvisionReadonlyUser idempotent: %v", err)
	}

	// Run migrations twice.
	driver, err := clickhouse.MigrateDriver(admin, cfg)
	if err != nil {
		t.Fatalf("MigrateDriver: %v", err)
	}
	if err := mmigrate.Run(ctx, "ch", driver, migrations.ClickHouse, "clickhouse", quietLogger()); err != nil {
		t.Fatalf("migrate first run: %v", err)
	}
	driver2, err := clickhouse.MigrateDriver(admin, cfg)
	if err != nil {
		t.Fatalf("MigrateDriver second: %v", err)
	}
	if err := mmigrate.Run(ctx, "ch", driver2, migrations.ClickHouse, "clickhouse", quietLogger()); err != nil {
		t.Fatalf("migrate second run (must be idempotent): %v", err)
	}

	// events_raw_v1 exists.
	var name string
	if err := admin.QueryRowContext(ctx,
		`SELECT name FROM system.tables WHERE database = ? AND name = 'events_raw_v1'`, cfg.ClickHouseDatabase,
	).Scan(&name); err != nil {
		t.Fatalf("events_raw_v1 not found: %v", err)
	}

	// Readonly user can SELECT but cannot INSERT.
	ro, err := clickhouse.OpenReadonly(ctx, cfg)
	if err != nil {
		t.Fatalf("OpenReadonly: %v", err)
	}
	t.Cleanup(func() { _ = ro.Close() })

	var count uint64
	if err := ro.QueryRowContext(ctx,
		`SELECT count() FROM events_raw_v1`,
	).Scan(&count); err != nil {
		t.Fatalf("readonly SELECT: %v", err)
	}
	if count != 0 {
		t.Errorf("expected empty table, got count=%d", count)
	}

	_, err = ro.ExecContext(ctx,
		`INSERT INTO events_raw_v1 (project_id, event, timestamp, properties, extras)
		 VALUES ('00000000-0000-0000-0000-000000000000', 'test', now64(3), '{}', '{}')`)
	if err == nil {
		t.Fatal("readonly INSERT must fail with access-denied; got nil error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "access") &&
		!strings.Contains(strings.ToLower(err.Error()), "denied") &&
		!strings.Contains(strings.ToLower(err.Error()), "readonly") &&
		!strings.Contains(strings.ToLower(err.Error()), "not enough privilege") {
		t.Errorf("readonly INSERT error doesn't look like an access-denied error: %v", err)
	}
}
