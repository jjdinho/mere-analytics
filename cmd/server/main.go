// Command mere-server is the analytics application binary.
//
// Boot sequence (failure at any step aborts startup; kamal-proxy keeps routing
// to the prior version):
//
//	env → config.Load → pg.Open → migrate.Run(pg) →
//	  ch.OpenAdmin → ch.CreateDatabase → ch.ProvisionReadonlyUser →
//	  migrate.Run(ch) → ch.OpenReadonly → http.Serve
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jjdinho/mere-analytics/internal/auth"
	"github.com/jjdinho/mere-analytics/internal/clickhouse"
	"github.com/jjdinho/mere-analytics/internal/config"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/web"
	"github.com/jjdinho/mere-analytics/migrations"
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
	logger.Info("config loaded", "cfg", cfg)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --- Postgres ---
	t := time.Now()
	pgPool, err := postgres.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("pg open: %w", err)
	}
	defer pgPool.Close()
	logger.Info("pg open", "duration_ms", time.Since(t).Milliseconds())

	pgDriver, err := postgres.MigrateDriver(cfg)
	if err != nil {
		return fmt.Errorf("pg migrate driver: %w", err)
	}
	if err := mmigrate.Run(ctx, "pg", pgDriver, migrations.Postgres, "postgres", logger); err != nil {
		return err
	}

	// --- ClickHouse: bootstrap (create DB, provision readonly user) ---
	if err := clickhouse.CreateDatabase(ctx, cfg); err != nil {
		return fmt.Errorf("ch create db: %w", err)
	}

	t = time.Now()
	chAdmin, err := clickhouse.OpenAdmin(ctx, cfg)
	if err != nil {
		return fmt.Errorf("ch admin open: %w", err)
	}
	defer chAdmin.Close()
	logger.Info("ch admin open", "duration_ms", time.Since(t).Milliseconds())

	if err := clickhouse.ProvisionReadonlyUser(ctx, chAdmin, cfg); err != nil {
		return fmt.Errorf("ch provision readonly: %w", err)
	}

	chDriver, err := clickhouse.MigrateDriver(chAdmin, cfg)
	if err != nil {
		return fmt.Errorf("ch migrate driver: %w", err)
	}
	if err := mmigrate.Run(ctx, "ch", chDriver, migrations.ClickHouse, "clickhouse", logger); err != nil {
		return err
	}

	// --- ClickHouse readonly pool (now that the user exists + grants are applied) ---
	chReadonly, err := clickhouse.OpenReadonly(ctx, cfg)
	if err != nil {
		return fmt.Errorf("ch readonly open: %w", err)
	}
	defer chReadonly.Close()
	logger.Info("ch readonly open")

	// chReadonly is wired but unused until step 8 (query API). Pin reference
	// so the variable isn't flagged unused while still letting it close cleanly.
	_ = (*sql.DB)(chReadonly)

	// --- HTTP ---
	authSvc := auth.NewService(pgPool)
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: web.Handler(web.Options{
			AuthService:   authSvc,
			Logger:        logger,
			SecureCookies: cfg.SecureCookies,
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serveErr := make(chan error, 1)
	go func() {
		logger.Info("http listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelShutdown()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("http shutdown forced", "err", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http listen: %w", err)
		}
		return nil
	}
}
