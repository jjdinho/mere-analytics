package config

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	Port int `envconfig:"PORT" default:"8080"`

	PostgresHost     string `envconfig:"POSTGRES_HOST" required:"true"`
	PostgresPort     int    `envconfig:"POSTGRES_PORT" default:"5432"`
	PostgresDB       string `envconfig:"POSTGRES_DB" default:"mere"`
	PostgresUser     string `envconfig:"POSTGRES_USER" default:"mere"`
	PostgresPassword string `envconfig:"POSTGRES_PASSWORD" required:"true"`

	ClickHouseHost             string `envconfig:"CLICKHOUSE_HOST" required:"true"`
	ClickHousePort             int    `envconfig:"CLICKHOUSE_PORT" default:"9000"`
	ClickHouseDatabase         string `envconfig:"CLICKHOUSE_DATABASE" default:"analytics"`
	ClickHouseAdminUser        string `envconfig:"CLICKHOUSE_ADMIN_USER" default:"mere_admin"`
	ClickHouseAdminPassword    string `envconfig:"CLICKHOUSE_ADMIN_PASSWORD" required:"true"`
	ClickHouseReadonlyUser     string `envconfig:"CLICKHOUSE_READONLY_USER" default:"mere_readonly"`
	ClickHouseReadonlyPassword string `envconfig:"CLICKHOUSE_READONLY_PASSWORD" required:"true"`
}

func Load() (Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return c, fmt.Errorf("envconfig: %w", err)
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
	)
}
