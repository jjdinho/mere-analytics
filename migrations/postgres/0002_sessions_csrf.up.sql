-- Per-session CSRF token. Step 3 (auth) is the first writer; existing rows
-- (none in v1) take the temporary default '' so the column lands NOT NULL,
-- then the default is dropped so future inserts must supply a value.
-- Backward-compatible: add-column with default → drop-default (see TODOS.md).

ALTER TABLE sessions ADD COLUMN csrf_token TEXT NOT NULL DEFAULT '';
ALTER TABLE sessions ALTER COLUMN csrf_token DROP DEFAULT;
