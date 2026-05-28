// Package postgres opens the application's Postgres connection pool and
// exposes a golang-migrate driver constructor for use by internal/migrate.
package postgres

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/golang-migrate/migrate/v4/database"
	pgmigrate "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" driver for database/sql

	"github.com/jjdinho/mere-analytics/internal/config"
)

// DSN builds the libpq-style DSN from the config.
func DSN(cfg config.Config) string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.PostgresUser, cfg.PostgresPassword,
		cfg.PostgresHost, cfg.PostgresPort, cfg.PostgresDB)
}

// Open returns a connected pgx pool, with a Ping verified.
func Open(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, DSN(cfg))
	if err != nil {
		return nil, fmt.Errorf("pgxpool new: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg ping: %w", err)
	}
	return pool, nil
}

// MigrateDriver opens a *sql.DB via the pgx stdlib shim and wraps it as a
// golang-migrate database.Driver. The returned driver owns its own *sql.DB;
// the migrate runner closes it via the underlying source-close path. Callers
// should not Close the returned driver themselves (migrate.Run closes it).
func MigrateDriver(cfg config.Config) (database.Driver, error) {
	db, err := sql.Open("pgx", DSN(cfg))
	if err != nil {
		return nil, fmt.Errorf("sql open: %w", err)
	}
	driver, err := pgmigrate.WithInstance(db, &pgmigrate.Config{})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("pg migrate driver: %w", err)
	}
	return driver, nil
}
