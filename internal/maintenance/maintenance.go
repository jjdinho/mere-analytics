// Package maintenance sweeps expired rows out of operational Postgres tables
// that would otherwise grow monotonically: oauth_codes, oauth_access_tokens,
// and sessions. It is the library half of cmd/maintenance and is invoked as
// a one-shot from host cron / Kamal scheduled task — not as a goroutine in
// the main server process.
//
// Scope is intentionally narrow: "expired" means expires_at < NOW(). Revoked
// access tokens that have not yet expired are left alone so a future audit
// view can surface them; oauth_clients are never touched.
package maintenance

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jjdinho/mere-analytics/internal/postgres/db"
)

// Result reports how many rows the sweep deleted per table. Callers use it
// for structured logging and exit-code decisions.
type Result struct {
	OAuthCodes        int64
	OAuthAccessTokens int64
	Sessions          int64
}

// Run executes the three cleanup queries in series and logs per-table counts.
// The queries are independent — a failure in one does not roll the others
// back; the function returns the first error it sees so a scheduled job
// surfaces it via its exit code.
func Run(ctx context.Context, pool *pgxpool.Pool, logger *slog.Logger) (Result, error) {
	q := db.New(pool)
	var res Result

	codes, err := q.DeleteExpiredOAuthCodes(ctx)
	if err != nil {
		return res, fmt.Errorf("delete expired oauth_codes: %w", err)
	}
	res.OAuthCodes = codes

	tokens, err := q.DeleteExpiredOAuthAccessTokens(ctx)
	if err != nil {
		return res, fmt.Errorf("delete expired oauth_access_tokens: %w", err)
	}
	res.OAuthAccessTokens = tokens

	sessions, err := q.DeleteExpiredSessions(ctx)
	if err != nil {
		return res, fmt.Errorf("delete expired sessions: %w", err)
	}
	res.Sessions = sessions

	logger.Info("maintenance sweep complete",
		"oauth_codes", res.OAuthCodes,
		"oauth_access_tokens", res.OAuthAccessTokens,
		"sessions", res.Sessions,
	)
	return res, nil
}
