// Package migrate is the shared migration runner used by both Postgres and
// ClickHouse. It wraps golang-migrate with per-migration timing, filename-aware
// error wrapping, and an operator-friendly runbook when the schema is left
// dirty by a prior failed apply.
//
// Boot wiring:
//
//	┌─────────────┐   driver  ┌──────────────┐
//	│ postgres or │──────────▶│  migrate.Run │──▶ Up()/ErrNoChange
//	│ clickhouse  │           │              │──▶ ErrDirty → runbook
//	│   pkg       │   fs.FS   │              │──▶ Logger →   slog
//	└─────────────┘──────────▶│              │
//	                          └──────────────┘
package migrate

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

// Run applies all pending migrations from fsys (rooted at root) using the
// supplied database driver. label identifies the dialect ("pg" or "ch") in
// logs and error messages.
//
// Behavior:
//   - migrate.ErrNoChange → success, logged at info
//   - migrate.ErrDirty    → returned with an operator runbook embedded in the message
//   - any other error     → wrapped with label and current schema version
func Run(ctx context.Context, label string, driver database.Driver, fsys fs.FS, root string, logger *slog.Logger) error {
	src, err := iofs.New(fsys, root)
	if err != nil {
		return fmt.Errorf("%s migrate: open source %q: %w", label, root, err)
	}
	defer src.Close()

	m, err := migrate.NewWithInstance("iofs", src, label, driver)
	if err != nil {
		return fmt.Errorf("%s migrate: new instance: %w", label, err)
	}
	m.Log = &slogBridge{logger: logger.With("dialect", label)}

	start := time.Now()
	preVersion, preDirty, _ := m.Version()
	if preDirty {
		return dirtyError(label, preVersion)
	}

	err = m.Up()
	switch {
	case errors.Is(err, migrate.ErrNoChange):
		logger.Info("migrate: no change", "dialect", label, "version", preVersion)
		return nil
	case err != nil:
		var derr migrate.ErrDirty
		if errors.As(err, &derr) {
			return dirtyError(label, uint(derr.Version))
		}
		postVersion, _, _ := m.Version()
		return fmt.Errorf("%s migrate: up failed at version %d: %w", label, postVersion, err)
	}

	postVersion, _, _ := m.Version()
	logger.Info("migrate: complete",
		"dialect", label,
		"from_version", preVersion,
		"to_version", postVersion,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

func dirtyError(label string, version uint) error {
	runbook := fmt.Sprintf(
		"%s migrate: version %d is DIRTY (a prior run failed mid-apply). "+
			"Inspect the schema then force the version with: "+
			"`migrate -path migrations/%s -database <dsn> force %d` "+
			"(or use the corresponding kamal db/clickhouse-console alias).",
		label, version, dialectDir(label), version)
	return errors.New(runbook)
}

func dialectDir(label string) string {
	switch strings.ToLower(label) {
	case "pg", "postgres", "postgresql":
		return "postgres"
	case "ch", "clickhouse":
		return "clickhouse"
	default:
		return label
	}
}

// slogBridge implements migrate.Logger by forwarding to slog at info level.
type slogBridge struct{ logger *slog.Logger }

func (s *slogBridge) Printf(format string, v ...any) {
	msg := strings.TrimRight(fmt.Sprintf(format, v...), "\n")
	s.logger.Info("migrate", "msg", msg)
}

func (s *slogBridge) Verbose() bool { return true }
