-- Dead-letter queue for events the flusher couldn't deliver to ClickHouse.
-- One row per failed flush, storing the entire validated batch as JSONB so
-- the drain goroutine can retry without losing event boundaries.
--
-- Drain progress: oldest-first via failed_events_drain_idx (partial WHERE
-- quarantined_at IS NULL). After 20 attempts OR 24h age the row quarantines
-- and is left alone; a future cmd/maintenance sweep reaps quarantined rows.

-- name: InsertFailedEvent :exec
-- Called by the flusher after a CH insert fails. id is a UUID v7 from
-- idgen.New(); batch_payload carries the validated Event slice as JSON; the
-- last_error column records the CH error string for forensics.
INSERT INTO failed_events (id, batch_payload, last_error)
VALUES ($1, $2, $3);

-- name: ListFailedEventsForDrain :many
-- Oldest-first slice for the drain goroutine. The LIMIT is bounded by
-- INGEST_DLQ_DRAIN_BATCH_LIMIT so a deep DLQ doesn't starve other work.
SELECT id, batch_payload, last_error, attempt_count, created_at,
       last_attempt_at, quarantined_at
FROM failed_events
WHERE quarantined_at IS NULL
ORDER BY created_at ASC
LIMIT $1;

-- name: IncrementFailedEventAttempt :exec
-- Recorded after a drain CH-insert attempt fails. The fresh last_error
-- displaces the previous one — only the most recent failure matters for
-- triage.
UPDATE failed_events
SET attempt_count = attempt_count + 1,
    last_attempt_at = NOW(),
    last_error = $2
WHERE id = $1;

-- name: DeleteFailedEvent :exec
-- Removes a row after a successful drain replay.
DELETE FROM failed_events WHERE id = $1;

-- name: QuarantineFailedEvent :exec
-- Marks a row as accepted-data-loss after the retry budget is exhausted
-- (>=20 attempts OR >24h age). The drain index excludes quarantined rows.
UPDATE failed_events
SET quarantined_at = NOW()
WHERE id = $1
  AND quarantined_at IS NULL;

-- name: CountActiveFailedEvents :one
-- Refreshes the in-process DLQ depth gauge after each drain pass.
SELECT COUNT(*)::BIGINT FROM failed_events WHERE quarantined_at IS NULL;
