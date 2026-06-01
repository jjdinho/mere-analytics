-- Adds the ingest dead-letter queue and the OAuth bearer's last-seen timestamp.
--
-- Add-only per the TODOS.md "migrations must be backward-compatible" rule:
-- the prior binary keeps booting (nothing it queries is removed) while the
-- new binary starts using both surfaces.
--
-- failed_events is the row-per-flush DLQ. One row per failed CH flush carries
-- the entire batch as JSONB (validated Events at the time of submission). The
-- partial index on (created_at) WHERE quarantined_at IS NULL keeps the drain
-- goroutine on an oldest-first index scan; quarantined rows stay until a
-- future cmd/maintenance sweep claims them.
--
-- oauth_access_tokens.last_used_at is updated fire-and-forget by RequireBearer
-- on every successful bearer lookup, throttled to ≥60s in SQL (see
-- UpdateOAuthAccessTokenLastUsed) so the WAL/lock cost is bounded on hot tokens.
-- Nullable: existing rows stay NULL until they're next used.

CREATE TABLE failed_events (
    id              UUID PRIMARY KEY,
    batch_payload   JSONB NOT NULL,
    last_error      TEXT NOT NULL,
    attempt_count   INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_attempt_at TIMESTAMPTZ,
    quarantined_at  TIMESTAMPTZ
);

CREATE INDEX failed_events_drain_idx
    ON failed_events(created_at)
    WHERE quarantined_at IS NULL;

ALTER TABLE oauth_access_tokens
    ADD COLUMN last_used_at TIMESTAMPTZ;
