// Package migrations exposes the application's SQL migration files as embedded
// filesystems. Both the runtime binary and integration tests import this
// package to get at the migration sources.
//
// The Postgres and ClickHouse FSes are rooted at "postgres/" and "clickhouse/"
// respectively — pass those subpaths to internal/migrate.Run.
package migrations

import "embed"

//go:embed postgres/*.sql
var Postgres embed.FS

//go:embed clickhouse/*.sql
var ClickHouse embed.FS
