// Package app is the importable composition root: the single exported entry
// point that builds and runs the fully-wired mere application. Everything else
// stays under internal/ (ADR-0003). A wrapper module (e.g. the private
// mere-cloud hosted layer) depends on exactly two exported packages — this one
// to build + run, and extension for the seam types — and never imports the
// internals directly.
//
// The boot sequence and the three-phase SIGTERM choreography are core behavior,
// so they live here rather than in cmd/server, which collapses to a thin shim.
package app

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

	"github.com/jjdinho/mere-analytics/extension"
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

// httpShutdownTimeout bounds SIGTERM phase 2 (drain in-flight HTTP handlers).
const httpShutdownTimeout = 10 * time.Second

// options holds the wrapper-supplied knobs. Defaults: stderr-less slog.Default
// logger, empty version, and the no-op extension seams (applied by web.Handler /
// ingest.NewService when nil is forwarded).
type options struct {
	logger      *slog.Logger
	version     string
	rateLimiter extension.RateLimiter
	usageSink   extension.UsageSink
	middleware  []func(http.Handler) http.Handler
}

// Option configures Build/Run. Adding options is public API and a potential
// breaking change for wrappers; keep the surface small.
type Option func(*options)

// WithLogger sets the structured logger. Defaults to slog.Default().
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithVersion sets the build stamp surfaced in the /healthz body. cmd/server
// forwards its -ldflags Version here.
func WithVersion(v string) Option { return func(o *options) { o.version = v } }

// WithRateLimiter injects the ingest/query/MCP rate-limit seam. Defaults to the
// no-op extension.AllowAll.
func WithRateLimiter(rl extension.RateLimiter) Option {
	return func(o *options) { o.rateLimiter = rl }
}

// WithUsageSink injects the ingest metering seam. Defaults to the no-op
// extension.Discard.
func WithUsageSink(us extension.UsageSink) Option {
	return func(o *options) { o.usageSink = us }
}

// WithHandlerMiddleware wraps the assembled root handler with edge middleware
// (e.g. a hosted layer's IP-level limiter or WAF). The first middleware listed
// is outermost: request flows mw[0] → mw[1] → … → core handler.
func WithHandlerMiddleware(mw ...func(http.Handler) http.Handler) Option {
	return func(o *options) { o.middleware = append(o.middleware, mw...) }
}

// App is a wired, not-yet-listening application: the http.Server plus the
// closers for its Postgres/ClickHouse pools and ingest pipeline.
type App struct {
	cfg     config.Config
	logger  *slog.Logger
	srv     *http.Server
	ingest  *ingest.Service
	closers []func()
}

// Build runs the full boot sequence (config load, DB open, migrations, CH
// bootstrap, service construction, handler assembly) and returns a wired App
// that is not yet listening. Config is read from the environment. On any
// failure, partially-opened resources are released before returning.
func Build(ctx context.Context, opts ...Option) (_ *App, err error) {
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	logger := o.logger
	if logger == nil {
		logger = slog.Default()
	}

	logger.Info("starting mere-server", "version", o.version)

	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	logger.Info("config loaded", "cfg", cfg)

	a := &App{cfg: cfg, logger: logger}
	// Release whatever we opened if Build fails partway through.
	defer func() {
		if err != nil {
			_ = a.Close()
		}
	}()

	// --- Postgres ---
	t := time.Now()
	pgPool, err := postgres.Open(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg open: %w", err)
	}
	a.closers = append(a.closers, pgPool.Close)
	logger.Info("pg open", "duration_ms", time.Since(t).Milliseconds())

	pgDriver, err := postgres.MigrateDriver(cfg)
	if err != nil {
		return nil, fmt.Errorf("pg migrate driver: %w", err)
	}
	if err = mmigrate.Run(ctx, "pg", pgDriver, migrations.Postgres, "postgres", logger); err != nil {
		return nil, err
	}

	// --- ClickHouse: bootstrap (create DB, provision readonly user) ---
	if err = clickhouse.CreateDatabase(ctx, cfg); err != nil {
		return nil, fmt.Errorf("ch create db: %w", err)
	}

	t = time.Now()
	chAdmin, err := clickhouse.OpenAdmin(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("ch admin open: %w", err)
	}
	a.closers = append(a.closers, func() { _ = chAdmin.Close() })
	logger.Info("ch admin open", "duration_ms", time.Since(t).Milliseconds())

	if err = clickhouse.ProvisionReadonlyUser(ctx, chAdmin, cfg); err != nil {
		return nil, fmt.Errorf("ch provision readonly: %w", err)
	}

	chDriver, err := clickhouse.MigrateDriver(chAdmin, cfg)
	if err != nil {
		return nil, fmt.Errorf("ch migrate driver: %w", err)
	}
	if err = mmigrate.Run(ctx, "ch", chDriver, migrations.ClickHouse, "clickhouse", logger); err != nil {
		return nil, err
	}

	// --- ClickHouse readonly pool (now that the user exists + grants are applied) ---
	chReadonly, err := clickhouse.OpenReadonly(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("ch readonly open: %w", err)
	}
	a.closers = append(a.closers, func() { _ = chReadonly.Close() })
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
		UsageSink:            o.usageSink,
	}, logger)
	ingestSvc.Start(ctx)
	a.ingest = ingestSvc
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

	var handler http.Handler = web.Handler(web.Options{
		AuthService:          authSvc,
		OAuthService:         oauthSvc,
		OAuthIssuer:          cfg.OAuthIssuerURL,
		Version:              o.version,
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
		RateLimiter:          o.rateLimiter,
	})
	// Wrap with wrapper-supplied edge middleware, first-listed outermost.
	for i := len(o.middleware) - 1; i >= 0; i-- {
		handler = o.middleware[i](handler)
	}

	a.srv = &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return a, nil
}

// Run = Build + ListenAndServe + the three-phase SIGTERM choreography. The
// passed ctx is augmented with SIGINT/SIGTERM handling, so a plain
// context.Background() from the entry point is enough.
func Run(ctx context.Context, opts ...Option) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	a, err := Build(ctx, opts...)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}

// Handler returns the assembled root handler, for serve-it-yourself / httptest
// callers (and Build-only tests).
func (a *App) Handler() http.Handler { return a.srv.Handler }

// Run serves until ctx is cancelled (SIGTERM) or the listener fails, then runs
// the three-phase shutdown so no event is silently dropped:
//
//	phase 1 — ingest.SetDisabled(true)   ; new ingest POSTs → 503
//	phase 2 — http.Shutdown               ; in-flight handlers finish enqueue
//	phase 3 — ingest.Shutdown(grace)      ; drain → CH, residual → DLQ
//
// Pools/pipeline are released on return via Close.
func (a *App) Run(ctx context.Context) error {
	defer a.Close()

	serveErr := make(chan error, 1)
	go func() {
		a.logger.Info("http listening", "addr", a.srv.Addr)
		if err := a.srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
		close(serveErr)
	}()

	select {
	case <-ctx.Done():
		a.logger.Info("shutting down")
		a.ingest.Flags().SetDisabled(true) // phase 1

		httpCtx, cancelHTTP := context.WithTimeout(context.Background(), httpShutdownTimeout)
		defer cancelHTTP()
		if err := a.srv.Shutdown(httpCtx); err != nil { // phase 2
			a.logger.Warn("http shutdown forced", "err", err)
		}

		ingestCtx, cancelIngest := context.WithTimeout(context.Background(), a.cfg.IngestShutdownGrace)
		defer cancelIngest()
		if err := a.ingest.Shutdown(ingestCtx); err != nil { // phase 3
			a.logger.Warn("ingest shutdown", "err", err)
		}
		return nil
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http listen: %w", err)
		}
		return nil
	}
}

// Close releases the pools and ingest pipeline for Build-only callers (e.g.
// tests that use Handler without Run). It first drains the ingest goroutines so
// they don't touch a closed ClickHouse pool, then closes the pools in reverse
// open order. Idempotent: a second call (e.g. after Run already drained ingest)
// is a no-op for the pipeline.
func (a *App) Close() error {
	if a.ingest != nil {
		ctx, cancel := context.WithTimeout(context.Background(), a.cfg.IngestShutdownGrace)
		defer cancel()
		_ = a.ingest.Shutdown(ctx)
	}
	for i := len(a.closers) - 1; i >= 0; i-- {
		a.closers[i]()
	}
	a.closers = nil
	return nil
}
