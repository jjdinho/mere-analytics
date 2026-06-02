package clickhouse

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/jjdinho/mere-analytics/internal/config"
)

// CreateDatabase runs `CREATE DATABASE IF NOT EXISTS <dbname>` as admin.
// Idempotent. Uses a connection that does NOT pre-select a database, since
// the database may not exist yet on first boot.
func CreateDatabase(ctx context.Context, cfg config.Config) error {
	db, err := openAdminNoDB(ctx, cfg)
	if err != nil {
		return fmt.Errorf("ch create db open: %w", err)
	}
	defer db.Close()

	stmt := fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s", quoteIdent(cfg.ClickHouseDatabase))
	if _, err := db.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("ch create db: %w", err)
	}
	return nil
}

// ProvisionReadonlyUser makes the readonly user's state deterministic on every
// boot:
//
//	CREATE USER IF NOT EXISTS <user> IDENTIFIED ... SETTINGS readonly=2
//	ALTER  USER             <user> IDENTIFIED ... SETTINGS readonly=2 -- forces pw/settings
//	GRANT  SELECT ON <db>.* TO <user>                                 -- idempotent
//	CREATE ROW POLICY OR REPLACE ... ON system.tables USING 0 TO <user>
//
// Eliminates silent divergence if a pre-existing user has wrong password,
// readonly setting, or grants (review finding 2.1 / decision 5).
//
// readonly=2 keeps DDL/DML blocked but allows the app to attach per-request
// SELECT settings such as additional_table_filters, max_result_rows, and
// max_execution_time. The built-in readonly profile is too strict for Step 6.
//
// system.tables is in ClickHouse's hardcoded always-accessible allowlist, so it
// cannot be revoked or gated by select_from_system_db_requires_grant. Its
// total_rows/total_bytes columns expose global counts across every project — a
// cross-tenant aggregate leak. A USING 0 row policy hides all of its rows from
// the readonly user; unlike an app-layer SQL blocklist it can't be bypassed by
// SELECT * or quoted identifiers. Schema introspection uses DESCRIBE, not
// system.tables, so it is unaffected.
func ProvisionReadonlyUser(ctx context.Context, db *sql.DB, cfg config.Config) error {
	user := quoteIdent(cfg.ClickHouseReadonlyUser)
	pw := quoteString(cfg.ClickHouseReadonlyPassword)
	dbName := quoteIdent(cfg.ClickHouseDatabase)
	policy := quoteIdent(cfg.ClickHouseReadonlyUser + "_hide_system_tables")

	stmts := []string{
		fmt.Sprintf("CREATE USER IF NOT EXISTS %s IDENTIFIED WITH sha256_password BY %s SETTINGS readonly=2", user, pw),
		fmt.Sprintf("ALTER USER %s IDENTIFIED WITH sha256_password BY %s SETTINGS readonly=2", user, pw),
		fmt.Sprintf("GRANT SELECT ON %s.* TO %s", dbName, user),
		fmt.Sprintf("CREATE ROW POLICY OR REPLACE %s ON system.tables USING 0 TO %s", policy, user),
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("ch provision readonly: %w", err)
		}
	}
	return nil
}

// quoteIdent wraps a ClickHouse identifier in backticks, doubling any inner
// backtick. Defensive even though our identifiers come from operator-controlled
// env vars.
func quoteIdent(s string) string {
	return "`" + strings.ReplaceAll(s, "`", "``") + "`"
}

// quoteString wraps a SQL string literal in single quotes, doubling any inner
// single quote.
func quoteString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
