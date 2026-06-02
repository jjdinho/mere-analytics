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
	"github.com/jjdinho/mere-analytics/internal/ingest"
	"github.com/jjdinho/mere-analytics/internal/mcp"
	mmigrate "github.com/jjdinho/mere-analytics/internal/migrate"
	"github.com/jjdinho/mere-analytics/internal/oauth"
	"github.com/jjdinho/mere-analytics/internal/postgres"
	"github.com/jjdinho/mere-analytics/internal/query"
	"github.com/jjdinho/mere-analytics/internal/web"
	"github.com/jjdinho/mere-analytics/migrations"
)

// Version is the build-time version stamp, injected via
// -ldflags="-X main.Version=$(git describe --tags --always --dirty)" by the
// Dockerfile / CI. It defaults to "dev" for plain `go build` / `go run`. The
// value is logged on boot and returned in the /healthz JSON body so operators
// can answer "what's deployed right now?" from `kamal logs` or a probe.
var Version = "dev"

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	logger.Info("starting mere-server", "version", Version)

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
	queryExec := query.NewExecutor(chReadonly, cfg.ClickHouseDatabase)
	queryExec.MaxResultRows = cfg.QueryMaxResultRows
	queryExec.MaxExecutionTime = int((cfg.QueryMaxExecutionTime + time.Second - 1) / time.Second)
	querySchema := query.NewSchemaProvider(chReadonly, queryExec)

	// --- Ingest pipeline ---
	ingestSvc := ingest.NewService(pgPool, chAdmin, ingest.Options{
		EventBuffer:          cfg.IngestEventBuffer,
		FlushEvents:          cfg.IngestFlushEvents,
		FlushInterval:        cfg.IngestFlushInterval,
		ShutdownGrace:        cfg.IngestShutdownGrace,
		Disabled:             cfg.IngestDisabled,
		MaxBodyBytes:         cfg.IngestMaxBodyBytes,
		DLQDrainBatchLimit:   cfg.IngestDLQDrainBatchLimit,
		DLQDepth503Threshold: cfg.DLQDepth503Threshold,
	}, logger)
	ingestSvc.Start(ctx)
	if cfg.IngestDisabled {
		logger.Warn("ingest disabled at boot (INGEST_DISABLED=true)")
	}

	// --- HTTP ---
	authSvc := auth.NewService(pgPool)
	oauthSvc := oauth.NewService(pgPool)
	oauthSvc.AccessTokenTTL = cfg.OAuthAccessTokenTTL
	oauthSvc.AuthorizationCodeTTL = cfg.OAuthAuthorizationCodeTTL
	mcpHandler := mcp.NewHTTPHandler(mcp.Deps{
		AuthService: authSvc,
		Executor:    queryExec,
		Schema:      querySchema,
		Logger:      logger,
	})
	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: web.Handler(web.Options{
			AuthService:          authSvc,
			OAuthService:         oauthSvc,
			OAuthIssuer:          cfg.OAuthIssuerURL,
			Version:              Version,
			Logger:               logger,
			SecureCookies:        cfg.SecureCookies,
			IngestService:        ingestSvc,
			AllowedOrigins:       cfg.AllowedOrigins,
			IngestMaxBodyBytes:   cfg.IngestMaxBodyBytes,
			DLQDepth503Threshold: cfg.DLQDepth503Threshold,
			QueryExecutor:        queryExec,
			QuerySchema:          querySchema,
			QueryMaxBodyBytes:    cfg.QueryMaxBodyBytes,
			MCPHandler:           mcpHandler,
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

	// SIGTERM choreography — three phases so no event is silently dropped.
	//
	//   SIGTERM
	//      │
	//      ▼
	//   phase 1 — ingestSvc.Flags().SetDisabled(true)  ; new /api/v1/ingest/events → 503
	//      │
	//      ▼
	//   phase 2 — srv.Shutdown(httpCtx 10s)             ; in-flight handlers
	//      │                                              complete enqueue
	//      ▼
	//   phase 3 — ingestSvc.Shutdown(grace)             ; close input gate,
	//                                                     drain → CH, residual → DLQ
	//      │
	//      ▼
	//   run() returns ──► deferred pgPool.Close / chAdmin.Close run
	select {
	case <-ctx.Done():
		logger.Info("shutting down")
		ingestSvc.Flags().SetDisabled(true) // phase 1

		httpCtx, cancelHTTP := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelHTTP()
		if err := srv.Shutdown(httpCtx); err != nil { // phase 2
			logger.Warn("http shutdown forced", "err", err)
		}

		ingestCtx, cancelIngest := context.WithTimeout(context.Background(), cfg.IngestShutdownGrace)
		defer cancelIngest()
		if err := ingestSvc.Shutdown(ingestCtx); err != nil { // phase 3
			logger.Warn("ingest shutdown", "err", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http listen: %w", err)
		}
		return nil
	}
}
