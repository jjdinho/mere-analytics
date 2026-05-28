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
//	CREATE USER IF NOT EXISTS <user> IDENTIFIED ... SETTINGS PROFILE 'readonly'
//	ALTER  USER             <user> IDENTIFIED ... SETTINGS PROFILE 'readonly'  -- forces pw/profile
//	GRANT  SELECT ON <db>.* TO <user>                                          -- idempotent
//
// Eliminates silent divergence if a pre-existing user has wrong password,
// profile, or grants (review finding 2.1 / decision 5).
func ProvisionReadonlyUser(ctx context.Context, db *sql.DB, cfg config.Config) error {
	user := quoteIdent(cfg.ClickHouseReadonlyUser)
	pw := quoteString(cfg.ClickHouseReadonlyPassword)
	dbName := quoteIdent(cfg.ClickHouseDatabase)

	stmts := []string{
		fmt.Sprintf("CREATE USER IF NOT EXISTS %s IDENTIFIED WITH sha256_password BY %s SETTINGS PROFILE 'readonly'", user, pw),
		fmt.Sprintf("ALTER USER %s IDENTIFIED WITH sha256_password BY %s SETTINGS PROFILE 'readonly'", user, pw),
		fmt.Sprintf("GRANT SELECT ON %s.* TO %s", dbName, user),
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
