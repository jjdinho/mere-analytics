ALTER TABLE oauth_access_tokens DROP COLUMN last_used_at;

DROP INDEX IF EXISTS failed_events_drain_idx;
DROP TABLE IF EXISTS failed_events;
