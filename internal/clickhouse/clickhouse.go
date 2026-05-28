// Package clickhouse opens the application's ClickHouse connection pools
// (admin + readonly) and exposes a golang-migrate driver constructor. The
// bootstrap.go file in this package owns the one-time-per-boot DDL needed
// to make a fresh CH instance ready for the app (CREATE DATABASE, provision
// the readonly user).
package clickhouse

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/golang-migrate/migrate/v4/database"
	chmigrate "github.com/golang-migrate/migrate/v4/database/clickhouse"

	"github.com/jjdinho/mere-analytics/internal/config"
)

func openWith(ctx context.Context, cfg config.Config, user, password, database string) (*sql.DB, error) {
	opts := &clickhouse.Options{
		Addr: []string{fmt.Sprintf("%s:%d", cfg.ClickHouseHost, cfg.ClickHousePort)},
		Auth: clickhouse.Auth{
			Database: database,
			Username: user,
			Password: password,
		},
	}
	db := clickhouse.OpenDB(opts)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ch ping (%s): %w", user, err)
	}
	return db, nil
}

// OpenAdmin returns a connected *sql.DB authenticated as the admin user,
// scoped to the analytics database.
func OpenAdmin(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	return openWith(ctx, cfg, cfg.ClickHouseAdminUser, cfg.ClickHouseAdminPassword, cfg.ClickHouseDatabase)
}

// OpenReadonly returns a connected *sql.DB authenticated as the readonly user.
// Must be called after ProvisionReadonlyUser has succeeded for this boot.
func OpenReadonly(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	return openWith(ctx, cfg, cfg.ClickHouseReadonlyUser, cfg.ClickHouseReadonlyPassword, cfg.ClickHouseDatabase)
}

// openAdminNoDB is used during bootstrap: connect without specifying the
// target database, because the database itself may not yet exist on first boot.
func openAdminNoDB(ctx context.Context, cfg config.Config) (*sql.DB, error) {
	return openWith(ctx, cfg, cfg.ClickHouseAdminUser, cfg.ClickHouseAdminPassword, "")
}

// MigrateDriver wraps an admin *sql.DB as a golang-migrate driver scoped to
// cfg.ClickHouseDatabase. The caller still owns the *sql.DB it passes in;
// migrate.Run closes the driver but not the underlying DB (matches the
// clickhouse migrate driver's behavior).
func MigrateDriver(db *sql.DB, cfg config.Config) (database.Driver, error) {
	driver, err := chmigrate.WithInstance(db, &chmigrate.Config{
		DatabaseName:          cfg.ClickHouseDatabase,
		MigrationsTable:       "schema_migrations",
		MultiStatementEnabled: true,
	})
	if err != nil {
		return nil, fmt.Errorf("ch migrate driver: %w", err)
	}
	return driver, nil
}
