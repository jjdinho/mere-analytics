// Command mere-maintenance sweeps expired oauth_codes, oauth_access_tokens,
// and sessions rows out of Postgres. It is a one-shot binary intended for
// invocation via host cron / Kamal scheduled task — not a long-running
// daemon. Failure surfaces via a non-zero exit code; success logs a single
// JSON line summarising per-table counts.
//
// The binary reuses internal/config so it picks up the same Postgres
// connection settings as cmd/server; ClickHouse + OAuth issuer vars must be
// present in the environment but are unused. In the production Kamal
// container they are already set, so this is invisible.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jjdinho/mere-analytics/internal/config"
	"github.com/jjdinho/mere-analytics/internal/maintenance"
	"github.com/jjdinho/mere-analytics/internal/postgres"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	pool, err := postgres.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("pg open: %w", err)
	}
	defer pool.Close()

	if _, err := maintenance.Run(ctx, pool, logger); err != nil {
		return err
	}
	return nil
}
