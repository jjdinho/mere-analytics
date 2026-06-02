package query_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	appch "github.com/jjdinho/mere-analytics/internal/clickhouse"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/query"
	"github.com/jjdinho/mere-analytics/internal/testhelpers"
	"github.com/jjdinho/mere-analytics/migrations"
)

// TestReadonlyUserSecurityProbe is an adversarial probe of the readonly
// ClickHouse user. Tenant isolation in this app rests entirely on (a) the
// `additional_table_filters` setting the Executor injects per request and
// (b) the readonly user's grants. This test attacks that perimeter directly,
// as the real readonly user, exercising the production provisioning path
// (ProvisionReadonlyUser -> OpenReadonly -> Executor).
//
// Each subtest asserts the SECURE expectation and logs ClickHouse's actual
// response, so one run reveals which vectors are already safe versus live.
// Subtests use t.Errorf (not Fatalf) so a single run surfaces every gap.
//
//	#1/#4  user-supplied SETTINGS overriding additional_table_filters
//	#2     dangerous table functions (remote/url/file/s3)
//	#3     system.* and information_schema reads
//	#6     write/DDL operations (INSERT/UPDATE/DELETE/DROP/TRUNCATE/ALTER/CREATE)
func TestReadonlyUserSecurityProbe(t *testing.T) {
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
		properties  string
	}{
		{projectA, "anon-a", `{"marker":"only-a"}`},
		{projectB, "anon-b", `{"marker":"only-b"}`},
	} {
		if _, err := admin.ExecContext(ctx, `
			INSERT INTO events_raw_v1
			(project_id, event, anonymous_id, user_id, timestamp, session_id, properties, extras)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		`, row.projectID, "pageview", row.anonymousID, nil, ts, "sess-"+row.anonymousID, row.properties, `{}`); err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}

	readonly, err := appch.OpenReadonly(ctx, cfg)
	if err != nil {
		t.Fatalf("open readonly: %v", err)
	}
	defer readonly.Close()
	exec := query.NewExecutor(readonly, cfg.ClickHouseDatabase)

	rawTable := cfg.ClickHouseDatabase + ".events_raw_v1"

	// ---- #1/#4: user-supplied SETTINGS must not override the injected filter ----
	//
	// The Executor injects additional_table_filters scoping events_raw_v1 to
	// project A. We append a SETTINGS clause that tries to replace that filter
	// with `1=1` (no restriction). If ClickHouse lets the in-SQL setting win,
	// project B's marker leaks through. This is the critical vector.
	t.Run("settings_override_additional_table_filters", func(t *testing.T) {
		evil := fmt.Sprintf(
			`SELECT properties FROM events_raw_v1 WHERE event = 'pageview' `+
				`SETTINGS additional_table_filters = {'%s': '1=1'}`,
			rawTable,
		)
		res, err := exec.Collect(ctx, projectA, evil, 100)
		if err != nil {
			if !errors.Is(err, query.ErrSettingsNotAllowed) {
				t.Errorf("query with user SETTINGS errored, but not via the app-layer guard: %v", err)
			}
			return
		}
		// No error means the guard did not fire: only safe if nothing leaked.
		t.Logf("OBSERVED: query with user SETTINGS succeeded, %d row(s): %v", len(res.Rows), res.Rows)
		for _, row := range res.Rows {
			got, _ := row[0].(string)
			if strings.Contains(got, "only-b") {
				t.Errorf("LEAK (#1/#4): user-supplied SETTINGS overrode the injected filter; project B visible: %v", res.Rows)
			}
		}
	})

	// ---- #2: dangerous table functions must be denied by grants ----
	//
	// remote() is the worst case: it can reconnect to the same server as a
	// different user, bypassing the readonly grant and the filter entirely.
	// url()/file()/s3() enable exfiltration. With only GRANT SELECT, source
	// privileges are not held, so these must fail before any I/O.
	t.Run("table_functions_denied", func(t *testing.T) {
		for _, tc := range []struct{ name, sql string }{
			{"remote", fmt.Sprintf(`SELECT count() FROM remote('127.0.0.1:9000', '%s', 'events_raw_v1')`, cfg.ClickHouseDatabase)},
			{"url", `SELECT * FROM url('http://127.0.0.1:1/x', CSV, 'a String')`},
			{"file", `SELECT * FROM file('x.csv', CSV, 'a String')`},
			{"s3", `SELECT * FROM s3('http://127.0.0.1:1/x', CSV, 'a String')`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := readonly.QueryContext(ctx, tc.sql)
				if err == nil {
					t.Errorf("LIVE (#2): %s table function was allowed; expected access denied", tc.name)
					return
				}
				t.Logf("OBSERVED: %s -> %v", tc.name, err)
				if !looksLikeAccessDenied(err) {
					t.Errorf("WARN (#2): %s failed but not with an access/privilege error (function may not be blocked by grants): %v", tc.name, err)
				}
			})
		}
	})

	// ---- #3: dangerous system tables must be denied ----
	//
	// additional_table_filters only scopes the named analytics tables; system
	// tables are unfiltered. system.query_log would leak other projects' query
	// text and literals; system.parts leaks per-project row counts;
	// system.processes leaks concurrent queries. With only GRANT SELECT ON
	// analytics.*, ClickHouse denies these (it requires a grant to read the
	// system DB). This guards against a future version flipping that default.
	t.Run("dangerous_system_tables_denied", func(t *testing.T) {
		for _, tc := range []struct{ name, sql string }{
			{"system.query_log", `SELECT count() FROM system.query_log`},
			{"system.parts", `SELECT count() FROM system.parts`},
			{"system.processes", `SELECT count() FROM system.processes`},
			{"system.users", `SELECT count() FROM system.users`},
			{"information_schema.tables", `SELECT count() FROM information_schema.tables`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var n uint64
				err := readonly.QueryRowContext(ctx, tc.sql).Scan(&n)
				if err == nil {
					t.Errorf("LIVE (#3): %s is readable (count=%d); expected access denied", tc.name, n)
					return
				}
				t.Logf("OBSERVED: %s -> %v", tc.name, err)
				if !looksLikeAccessDenied(err) {
					t.Logf("NOTE (#3): %s failed but not with an access/privilege error: %v", tc.name, err)
				}
			})
		}
	})

	// system.tables (and sibling introspection tables) are in ClickHouse's
	// hardcoded always-accessible allowlist: they cannot be denied via grants or
	// the select_from_system_db_requires_grant config, and a string blocklist on
	// the column names is bypassable (SELECT * returns total_rows/total_bytes
	// without naming them) and dodgeable via quoted identifiers. The robust fix
	// is a ROW POLICY (USING 0) provisioned for the readonly user, which hides
	// every row regardless of query shape. Asserting zero rows confirms the
	// global total_rows/total_bytes cross-tenant count leak is closed.
	t.Run("system_tables_rows_hidden_from_readonly", func(t *testing.T) {
		var n uint64
		if err := readonly.QueryRowContext(ctx, `SELECT count() FROM system.tables`).Scan(&n); err != nil {
			// Denied outright (a future ClickHouse) is even safer.
			t.Logf("OBSERVED: system.tables denied outright: %v", err)
			return
		}
		t.Logf("OBSERVED: system.tables exposes %d row(s) to the readonly user", n)
		if n != 0 {
			t.Errorf("LEAK (#3): system.tables exposed %d row(s); the row policy should hide all rows (total_rows/total_bytes leak global cross-tenant counts)", n)
		}
	})

	// ---- #6: writes and DDL must be denied by readonly=2 + SELECT-only grant ----
	t.Run("ddl_dml_denied", func(t *testing.T) {
		for _, tc := range []struct{ name, sql string }{
			{"INSERT", `INSERT INTO events_raw_v1 (project_id, event, timestamp, properties, extras) VALUES ('00000000-0000-0000-0000-000000000000', 'x', now64(3), '{}', '{}')`},
			{"UPDATE", `ALTER TABLE events_raw_v1 UPDATE event = 'x' WHERE 1 = 0`},
			{"DELETE", `ALTER TABLE events_raw_v1 DELETE WHERE 1 = 0`},
			{"DROP", `DROP TABLE events_raw_v1`},
			{"TRUNCATE", `TRUNCATE TABLE events_raw_v1`},
			{"ALTER", `ALTER TABLE events_raw_v1 ADD COLUMN evil String`},
			{"CREATE", `CREATE TABLE evil_probe (a UInt8) ENGINE = MergeTree ORDER BY a`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := readonly.ExecContext(ctx, tc.sql)
				if err == nil {
					t.Errorf("LIVE (#6): %s was allowed; expected access denied", tc.name)
					return
				}
				t.Logf("OBSERVED: %s -> %v", tc.name, err)
				if !looksLikeAccessDenied(err) {
					t.Errorf("WARN (#6): %s failed but not with an access/readonly error: %v", tc.name, err)
				}
			})
		}
	})
}

// looksLikeAccessDenied reports whether err reads like a ClickHouse
// authorization rejection rather than an execution/connection error. Used to
// distinguish "blocked by grants/readonly" (the secure outcome) from "tried to
// run and failed for another reason" (which would mean the guard didn't fire).
func looksLikeAccessDenied(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"access", "denied", "not enough privilege", "privilege", "readonly", "not allowed"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
