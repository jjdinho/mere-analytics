package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

type Config struct {
	Port int `env:"PORT" envDefault:"8080"`

	// SecureCookies controls the Secure flag on the session and CSRF cookies.
	// Defaults to true (TLS-terminating proxy in front in production). Set to
	// false locally to allow plaintext HTTP on 127.0.0.1.
	SecureCookies bool `env:"SECURE_COOKIES" envDefault:"true"`

	PostgresHost     string `env:"POSTGRES_HOST,required"`
	PostgresPort     int    `env:"POSTGRES_PORT" envDefault:"5432"`
	PostgresDB       string `env:"POSTGRES_DB" envDefault:"mere"`
	PostgresUser     string `env:"POSTGRES_USER" envDefault:"mere"`
	PostgresPassword string `env:"POSTGRES_PASSWORD,required"`

	ClickHouseHost             string `env:"CLICKHOUSE_HOST,required"`
	ClickHousePort             int    `env:"CLICKHOUSE_PORT" envDefault:"9000"`
	ClickHouseDatabase         string `env:"CLICKHOUSE_DATABASE" envDefault:"analytics"`
	ClickHouseAdminUser        string `env:"CLICKHOUSE_ADMIN_USER" envDefault:"mere_admin"`
	ClickHouseAdminPassword    string `env:"CLICKHOUSE_ADMIN_PASSWORD,required"`
	ClickHouseReadonlyUser     string `env:"CLICKHOUSE_READONLY_USER" envDefault:"mere_readonly"`
	ClickHouseReadonlyPassword string `env:"CLICKHOUSE_READONLY_PASSWORD,required"`

	// OAuthIssuerURL is the externally reachable base URL the OAuth server
	// advertises in its discovery document and signs authorization redirects
	// against. Required because the discovery JSON must be absolute.
	OAuthIssuerURL            string        `env:"OAUTH_ISSUER_URL,required"`
	OAuthAccessTokenTTL       time.Duration `env:"OAUTH_ACCESS_TOKEN_TTL" envDefault:"1h"`
	OAuthAuthorizationCodeTTL time.Duration `env:"OAUTH_AUTHORIZATION_CODE_TTL" envDefault:"10m"`

	// Ingest pipeline (Step 5). IngestEventBuffer is the atomic-pending event
	// ceiling Submit checks against; the underlying channel is sized smaller
	// because envelopes carry many events each. IngestFlushEvents +
	// IngestFlushInterval form the per-batch flush trigger (whichever fires
	// first). IngestShutdownGrace bounds phase 3 of SIGTERM. IngestDisabled
	// is the kill switch — when true, /api/v1/ingest/events 503s immediately without
	// touching the channel. IngestMaxBodyBytes caps the request body
	// surface area (10 MiB).
	IngestEventBuffer        int           `env:"INGEST_EVENT_BUFFER" envDefault:"50000"`
	IngestFlushEvents        int           `env:"INGEST_FLUSH_EVENTS" envDefault:"5000"`
	IngestFlushInterval      time.Duration `env:"INGEST_FLUSH_INTERVAL" envDefault:"2s"`
	IngestShutdownGrace      time.Duration `env:"INGEST_SHUTDOWN_GRACE" envDefault:"10s"`
	IngestDisabled           bool          `env:"INGEST_DISABLED" envDefault:"false"`
	IngestMaxBodyBytes       int64         `env:"INGEST_MAX_BODY_BYTES" envDefault:"10485760"`
	IngestDLQDrainBatchLimit int           `env:"INGEST_DLQ_DRAIN_BATCH_LIMIT" envDefault:"10"`

	// DLQDepth503Threshold is the active failed_events row count above which
	// /healthz returns 503. Operators page on the resulting kamal-proxy
	// circuit-break.
	DLQDepth503Threshold int `env:"DLQ_DEPTH_503_THRESHOLD" envDefault:"100000"`

	// AllowedOrigins restricts the Access-Control-Allow-Origin header on
	// /api/v1/ingest/events and bearer API routes. Empty (default) → `*`. Comma-separated
	// list of exact origins (no wildcards beyond the empty-list case).
	AllowedOrigins []string `env:"ALLOWED_ORIGINS" envSeparator:"," envDefault:""`

	// QueryMaxBodyBytes caps POST /api/v1/projects/:id/query bodies. The query
	// result itself streams and is bounded by ClickHouse max_result_rows.
	QueryMaxBodyBytes int64 `env:"QUERY_MAX_BODY_BYTES" envDefault:"262144"`
}

func Load() (Config, error) {
	var c Config
	if err := env.Parse(&c); err != nil {
		return c, fmt.Errorf("env: %w", err)
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c Config) validate() error {
	var missing []string
	check := func(name, val string) {
		if strings.TrimSpace(val) == "" {
			missing = append(missing, name)
		}
	}
	check("POSTGRES_HOST", c.PostgresHost)
	check("POSTGRES_DB", c.PostgresDB)
	check("POSTGRES_USER", c.PostgresUser)
	check("POSTGRES_PASSWORD", c.PostgresPassword)
	check("CLICKHOUSE_HOST", c.ClickHouseHost)
	check("CLICKHOUSE_DATABASE", c.ClickHouseDatabase)
	check("CLICKHOUSE_ADMIN_USER", c.ClickHouseAdminUser)
	check("CLICKHOUSE_ADMIN_PASSWORD", c.ClickHouseAdminPassword)
	check("CLICKHOUSE_READONLY_USER", c.ClickHouseReadonlyUser)
	check("CLICKHOUSE_READONLY_PASSWORD", c.ClickHouseReadonlyPassword)
	check("OAUTH_ISSUER_URL", c.OAuthIssuerURL)
	if len(missing) > 0 {
		return fmt.Errorf("required env vars empty: %s", strings.Join(missing, ", "))
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("PORT %d out of range (1-65535)", c.Port)
	}
	if c.PostgresPort <= 0 || c.PostgresPort > 65535 {
		return fmt.Errorf("POSTGRES_PORT %d out of range", c.PostgresPort)
	}
	if c.ClickHousePort <= 0 || c.ClickHousePort > 65535 {
		return fmt.Errorf("CLICKHOUSE_PORT %d out of range", c.ClickHousePort)
	}
	return nil
}

// LogValue redacts password fields so logging the config doesn't leak secrets.
// If you add a new field to Config, add it here too — config_test.go enforces this.
func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int("port", c.Port),
		slog.Bool("secure_cookies", c.SecureCookies),
		slog.String("postgres_host", c.PostgresHost),
		slog.Int("postgres_port", c.PostgresPort),
		slog.String("postgres_db", c.PostgresDB),
		slog.String("postgres_user", c.PostgresUser),
		slog.String("postgres_password", "[REDACTED]"),
		slog.String("clickhouse_host", c.ClickHouseHost),
		slog.Int("clickhouse_port", c.ClickHousePort),
		slog.String("clickhouse_database", c.ClickHouseDatabase),
		slog.String("clickhouse_admin_user", c.ClickHouseAdminUser),
		slog.String("clickhouse_admin_password", "[REDACTED]"),
		slog.String("clickhouse_readonly_user", c.ClickHouseReadonlyUser),
		slog.String("clickhouse_readonly_password", "[REDACTED]"),
		slog.String("oauth_issuer_url", c.OAuthIssuerURL),
		slog.Duration("oauth_access_token_ttl", c.OAuthAccessTokenTTL),
		slog.Duration("oauth_authorization_code_ttl", c.OAuthAuthorizationCodeTTL),
		slog.Int("ingest_event_buffer", c.IngestEventBuffer),
		slog.Int("ingest_flush_events", c.IngestFlushEvents),
		slog.Duration("ingest_flush_interval", c.IngestFlushInterval),
		slog.Duration("ingest_shutdown_grace", c.IngestShutdownGrace),
		slog.Bool("ingest_disabled", c.IngestDisabled),
		slog.Int64("ingest_max_body_bytes", c.IngestMaxBodyBytes),
		slog.Int("ingest_dlq_drain_batch_limit", c.IngestDLQDrainBatchLimit),
		slog.Int("dlq_depth_503_threshold", c.DLQDepth503Threshold),
		slog.Any("allowed_origins", c.AllowedOrigins),
		slog.Int64("query_max_body_bytes", c.QueryMaxBodyBytes),
	)
}
