-- pgcrypto is required by scripts/operator/reset-password.sql, which uses
-- crypt(...) + gen_salt('bf', 10) so operator-written and app-written bcrypt
-- hashes are wire-compatible. Idempotent: extension already present in some
-- deploys via config/deploy/postgres/init.sql (step 10).
-- Backward-compatible: add-only, per TODOS.md convention.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
