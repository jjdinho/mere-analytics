CREATE TABLE IF NOT EXISTS events_raw_v1 (
    project_id   UUID,
    event        LowCardinality(String),
    distinct_id  Nullable(String),
    timestamp    DateTime64(3, 'UTC'),
    session_id   Nullable(String),
    properties   String,
    extras       String,
    received_at  DateTime64(3, 'UTC') DEFAULT now64(3)
) ENGINE = MergeTree
ORDER BY (project_id, timestamp, event)
PARTITION BY toYYYYMM(timestamp);
