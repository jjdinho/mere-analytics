package testhelpers

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/testcontainers/testcontainers-go"
	tcclickhouse "github.com/testcontainers/testcontainers-go/modules/clickhouse"

	"github.com/jjdinho/mere-analytics/internal/config"
)

// StartClickHouse boots a fresh clickhouse-server container with admin
// credentials matching what the app expects (mere_admin), and returns an
// admin-authenticated *sql.DB plus a Config pre-populated with the
// container's host/port. The readonly user is NOT pre-provisioned — that's
// what the app's ProvisionReadonlyUser is supposed to do, so the integration
// tests cover it.
func StartClickHouse(t *testing.T) (*sql.DB, config.Config) {
	t.Helper()
	ctx := context.Background()

	const (
		dbName       = "analytics"
		adminUser    = "mere_admin"
		adminPass    = "devpass"
		readonlyUser = "mere_readonly"
		readonlyPass = "devpass-ro"
	)

	container, err := tcclickhouse.Run(ctx,
		"clickhouse/clickhouse-server:24.12-alpine",
		tcclickhouse.WithUsername(adminUser),
		tcclickhouse.WithPassword(adminPass),
		tcclickhouse.WithDatabase(dbName),
		// Required so mere_admin can CREATE USER for ProvisionReadonlyUser.
		testcontainers.WithEnv(map[string]string{
			"CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT": "1",
		}),
	)
	if err != nil {
		t.Fatalf("start clickhouse container: %v", err)
	}
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := container.MappedPort(ctx, "9000/tcp")
	if err != nil {
		t.Fatalf("container port: %v", err)
	}

	cfg := config.Config{
		Port:                       8080,
		PostgresHost:               "unused.invalid",
		PostgresPort:               5432,
		PostgresDB:                 "mere",
		PostgresUser:               "mere",
		PostgresPassword:           "unused",
		ClickHouseHost:             host,
		ClickHousePort:             int(port.Num()),
		ClickHouseDatabase:         dbName,
		ClickHouseAdminUser:        adminUser,
		ClickHouseAdminPassword:    adminPass,
		ClickHouseReadonlyUser:     readonlyUser,
		ClickHouseReadonlyPassword: readonlyPass,
	}

	db := clickhouse.OpenDB(&clickhouse.Options{
		Addr: []string{fmt.Sprintf("%s:%d", cfg.ClickHouseHost, cfg.ClickHousePort)},
		Auth: clickhouse.Auth{
			Database: cfg.ClickHouseDatabase,
			Username: cfg.ClickHouseAdminUser,
			Password: cfg.ClickHouseAdminPassword,
		},
	})
	t.Cleanup(func() { _ = db.Close() })
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("clickhouse ping: %v", err)
	}

	return db, cfg
}
