package app_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/jjdinho/mere-analytics/app"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
)

// TestBuild_ServesAndCloses is the composition-root green gate: Build runs the
// full real boot sequence against live PG + CH and returns a wired App whose
// Handler serves /healthz, and Close releases the pools/pipeline cleanly
// (idempotently).
func TestBuild_ServesAndCloses(t *testing.T) {
	pgPool, pgCfg := testhelpers.StartPostgres(t)
	pgPool.Close() // Build opens its own pool from the env below
	_, chCfg := testhelpers.StartClickHouse(t)

	t.Setenv("PORT", "8080")
	t.Setenv("SECURE_COOKIES", "false")
	t.Setenv("POSTGRES_HOST", pgCfg.PostgresHost)
	t.Setenv("POSTGRES_PORT", strconv.Itoa(pgCfg.PostgresPort))
	t.Setenv("POSTGRES_DB", pgCfg.PostgresDB)
	t.Setenv("POSTGRES_USER", pgCfg.PostgresUser)
	t.Setenv("POSTGRES_PASSWORD", pgCfg.PostgresPassword)
	t.Setenv("CLICKHOUSE_HOST", chCfg.ClickHouseHost)
	t.Setenv("CLICKHOUSE_PORT", strconv.Itoa(chCfg.ClickHousePort))
	t.Setenv("CLICKHOUSE_DATABASE", chCfg.ClickHouseDatabase)
	t.Setenv("CLICKHOUSE_ADMIN_USER", chCfg.ClickHouseAdminUser)
	t.Setenv("CLICKHOUSE_ADMIN_PASSWORD", chCfg.ClickHouseAdminPassword)
	t.Setenv("CLICKHOUSE_READONLY_USER", chCfg.ClickHouseReadonlyUser)
	t.Setenv("CLICKHOUSE_READONLY_PASSWORD", chCfg.ClickHouseReadonlyPassword)
	t.Setenv("OAUTH_ISSUER_URL", "http://127.0.0.1:8080")

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a, err := app.Build(context.Background(), app.WithLogger(logger), app.WithVersion("vtest"))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d want 200; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Errorf("body missing ok status: %s", body)
	}
	if !strings.Contains(string(body), `"version":"vtest"`) {
		t.Errorf("body missing version stamp: %s", body)
	}

	if err := a.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Idempotent: a second Close must not panic or error.
	if err := a.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
