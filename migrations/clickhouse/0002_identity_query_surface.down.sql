DROP VIEW IF EXISTS sessions;
DROP VIEW IF EXISTS persons;
DROP VIEW IF EXISTS events;

DROP VIEW IF EXISTS sessions_mv;
DROP VIEW IF EXISTS persons_mv;
DROP VIEW IF EXISTS identity_links_mv;

DROP TABLE IF EXISTS sessions_state;
DROP TABLE IF EXISTS persons_state;
DROP TABLE IF EXISTS identity_links_v1;

ALTER TABLE events_raw_v1
    DROP COLUMN IF EXISTS user_id,
    DROP COLUMN IF EXISTS anonymous_id;

ALTER TABLE events_raw_v1
    ADD COLUMN IF NOT EXISTS distinct_id Nullable(String) AFTER event;
