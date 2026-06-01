// Package testhelpers provides shared testcontainers-go setup for PG and CH
// used by per-package integration tests and the e2e boot test.
//
// This package is never imported by production code, so it (and its testcontainers
// dependency tree) is absent from the final binary. The test-only build path
// is enforced socially — no //go:build tag — but a CI check could grep for
// production imports if drift becomes a worry.
package testhelpers

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/jjdinho/mere-analytics/internal/config"
)

// StartPostgres boots a fresh postgres:16 container, waits until it accepts
// connections, and returns a connected pgxpool plus a Config pre-populated
// with the container's host/port and credentials. The container and pool are
// torn down via t.Cleanup. The host port is ephemeral.
func StartPostgres(t *testing.T) (*pgxpool.Pool, config.Config) {
	t.Helper()
	pool, cfg, _ := startPostgres(t, 0)
	return pool, cfg
}

// StartPostgresC is StartPostgres plus the raw container handle, for tests that
// Stop/Start the dependency to exercise recovery paths. Unlike StartPostgres it
// pins a fixed host port, because Docker reassigns an ephemeral port on
// Stop/Start — pinning lets the returned pool reconnect on the same address
// after a restart.
func StartPostgresC(t *testing.T) (*pgxpool.Pool, config.Config, testcontainers.Container) {
	t.Helper()
	return startPostgres(t, freeHostPort(t))
}

// startPostgres is the shared bring-up. fixedPort==0 publishes 5432 to an
// ephemeral host port; a non-zero fixedPort pins it so it survives a Stop/Start
// cycle.
func startPostgres(t *testing.T, fixedPort int) (*pgxpool.Pool, config.Config, testcontainers.Container) {
	t.Helper()
	ctx := context.Background()

	const (
		dbName = "mere"
		dbUser = "mere"
		dbPass = "devpass"
	)

	opts := []testcontainers.ContainerCustomizer{
		tcpostgres.WithDatabase(dbName),
		tcpostgres.WithUsername(dbUser),
		tcpostgres.WithPassword(dbPass),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second)),
	}
	if fixedPort != 0 {
		opts = append(opts, pinHostPort(t, "5432/tcp", fixedPort))
	}

	container, err := tcpostgres.Run(ctx, "postgres:16-alpine", opts...)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "5432/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	cfg := config.Config{
		Port:                       8080,
		PostgresHost:               host,
		PostgresPort:               int(port.Num()),
		PostgresDB:                 dbName,
		PostgresUser:               dbUser,
		PostgresPassword:           dbPass,
		ClickHouseHost:             "unused.invalid",
		ClickHousePort:             9000,
		ClickHouseDatabase:         "analytics",
		ClickHouseAdminUser:        "mere_admin",
		ClickHouseAdminPassword:    "unused",
		ClickHouseReadonlyUser:     "mere_readonly",
		ClickHouseReadonlyPassword: "unused",
	}

	pool, err := pgxpool.New(ctx, dsnFromConfig(cfg))
	if err != nil {
		t.Fatalf("pgxpool new: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("pgxpool ping: %v", err)
	}

	return pool, cfg, container
}

func dsnFromConfig(c config.Config) string {
	// Avoid importing internal/postgres here to keep testhelpers' dep graph
	// independent of the package under test.
	return "postgres://" + c.PostgresUser + ":" + c.PostgresPassword +
		"@" + c.PostgresHost + ":" + itoa(c.PostgresPort) +
		"/" + c.PostgresDB + "?sslmode=disable"
}
