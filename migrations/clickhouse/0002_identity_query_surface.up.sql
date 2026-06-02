ALTER TABLE events_raw_v1
    DROP COLUMN IF EXISTS distinct_id;

ALTER TABLE events_raw_v1
    ADD COLUMN IF NOT EXISTS anonymous_id Nullable(String) AFTER event,
    ADD COLUMN IF NOT EXISTS user_id Nullable(String) AFTER anonymous_id;

CREATE TABLE IF NOT EXISTS identity_links_v1 (
    project_id   UUID,
    anonymous_id String,
    user_id      String,
    linked_at    DateTime64(3, 'UTC'),
    session_id   String,
    received_at  DateTime64(3, 'UTC')
) ENGINE = MergeTree
ORDER BY (project_id, anonymous_id, linked_at)
PARTITION BY toYYYYMM(linked_at);

CREATE TABLE IF NOT EXISTS persons_state (
    project_id      UUID,
    raw_distinct_id String,
    first_seen      SimpleAggregateFunction(min, DateTime64(3, 'UTC')),
    last_seen       SimpleAggregateFunction(max, DateTime64(3, 'UTC')),
    event_count     SimpleAggregateFunction(sum, UInt64),
    session_count   AggregateFunction(uniq, String),
    timezone        SimpleAggregateFunction(anyLast, LowCardinality(String))
) ENGINE = AggregatingMergeTree
ORDER BY (project_id, raw_distinct_id)
PARTITION BY toYYYYMM(first_seen);

CREATE TABLE IF NOT EXISTS sessions_state (
    project_id   UUID,
    session_id   String,
    anonymous_id SimpleAggregateFunction(any, String),
    user_id      SimpleAggregateFunction(anyLast, String),
    started_at   SimpleAggregateFunction(min, DateTime64(3, 'UTC')),
    ended_at     SimpleAggregateFunction(max, DateTime64(3, 'UTC')),
    event_count  SimpleAggregateFunction(sum, UInt64),
    timezone     SimpleAggregateFunction(anyLast, LowCardinality(String))
) ENGINE = AggregatingMergeTree
ORDER BY (project_id, session_id)
PARTITION BY toYYYYMM(started_at);

CREATE MATERIALIZED VIEW IF NOT EXISTS identity_links_mv
TO identity_links_v1
AS SELECT
    project_id,
    assumeNotNull(anonymous_id) AS anonymous_id,
    assumeNotNull(user_id) AS user_id,
    timestamp AS linked_at,
    ifNull(session_id, '') AS session_id,
    received_at
FROM events_raw_v1
WHERE event = '$identify'
  AND isNotNull(anonymous_id)
  AND anonymous_id != ''
  AND isNotNull(user_id)
  AND user_id != '';

CREATE MATERIALIZED VIEW IF NOT EXISTS persons_mv
TO persons_state
AS SELECT
    project_id,
    assumeNotNull(coalesce(nullIf(user_id, ''), nullIf(anonymous_id, ''))) AS raw_distinct_id,
    min(timestamp) AS first_seen,
    max(timestamp) AS last_seen,
    count() AS event_count,
    uniqStateIf(assumeNotNull(session_id), isNotNull(session_id) AND session_id != '') AS session_count,
    anyLastIf(toLowCardinality(JSONExtractString(properties, '$timezone')), JSONExtractString(properties, '$timezone') != '') AS timezone
FROM events_raw_v1
WHERE isNotNull(coalesce(nullIf(user_id, ''), nullIf(anonymous_id, '')))
GROUP BY project_id, raw_distinct_id;

CREATE MATERIALIZED VIEW IF NOT EXISTS sessions_mv
TO sessions_state
AS SELECT
    project_id,
    assumeNotNull(session_id) AS session_id,
    anyIf(assumeNotNull(anonymous_id), isNotNull(anonymous_id) AND anonymous_id != '') AS anonymous_id,
    anyLastIf(assumeNotNull(user_id), isNotNull(user_id) AND user_id != '') AS user_id,
    min(timestamp) AS started_at,
    max(timestamp) AS ended_at,
    count() AS event_count,
    anyLastIf(toLowCardinality(JSONExtractString(properties, '$timezone')), JSONExtractString(properties, '$timezone') != '') AS timezone
FROM events_raw_v1
WHERE isNotNull(session_id)
  AND session_id != ''
GROUP BY project_id, session_id;

CREATE VIEW IF NOT EXISTS events AS
SELECT
    e.project_id,
    e.event,
    coalesce(nullIf(e.user_id, ''), nullIf(il.user_id, ''), nullIf(e.anonymous_id, '')) AS distinct_id,
    e.timestamp,
    e.session_id,
    e.properties,
    e.extras,
    e.received_at
FROM events_raw_v1 AS e
LEFT JOIN (
    SELECT
        project_id,
        anonymous_id,
        argMin(user_id, linked_at) AS user_id
    FROM identity_links_v1
    GROUP BY project_id, anonymous_id
) AS il
    ON e.project_id = il.project_id
   AND e.anonymous_id = il.anonymous_id;

CREATE VIEW IF NOT EXISTS persons AS
SELECT
    p.project_id,
    coalesce(nullIf(il.user_id, ''), p.raw_distinct_id) AS distinct_id,
    min(p.first_seen) AS first_seen,
    max(p.last_seen) AS last_seen,
    sum(p.event_count) AS event_count,
    uniqMerge(p.session_count) AS session_count,
    anyLast(p.timezone) AS timezone
FROM persons_state AS p
LEFT JOIN (
    SELECT
        project_id,
        anonymous_id,
        argMin(user_id, linked_at) AS user_id
    FROM identity_links_v1
    GROUP BY project_id, anonymous_id
) AS il
    ON p.project_id = il.project_id
   AND p.raw_distinct_id = il.anonymous_id
GROUP BY p.project_id, distinct_id;

CREATE VIEW IF NOT EXISTS sessions AS
SELECT
    s.project_id,
    s.session_id,
    coalesce(nullIf(s.user_id, ''), nullIf(il.user_id, ''), nullIf(s.anonymous_id, '')) AS distinct_id,
    s.started_at,
    s.ended_at,
    dateDiff('millisecond', s.started_at, s.ended_at) AS duration_ms,
    s.event_count,
    s.timezone
FROM (
    SELECT
        project_id,
        session_id,
        any(anonymous_id) AS anonymous_id,
        anyLast(user_id) AS user_id,
        min(started_at) AS started_at,
        max(ended_at) AS ended_at,
        sum(event_count) AS event_count,
        anyLast(timezone) AS timezone
    FROM sessions_state
    GROUP BY project_id, session_id
) AS s
LEFT JOIN (
    SELECT
        project_id,
        anonymous_id,
        argMin(user_id, linked_at) AS user_id
    FROM identity_links_v1
    GROUP BY project_id, anonymous_id
) AS il
    ON s.project_id = il.project_id
   AND s.anonymous_id = il.anonymous_id;
