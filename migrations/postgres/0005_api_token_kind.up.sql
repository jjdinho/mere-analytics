-- Token kinds: the same api_tokens table now holds two flavours of bearer.
--
--   public_ingest — handed out in the JS snippet, dereferenced server-side to
--                   stamp project_id on incoming events. Plaintext is *not*
--                   secret; it sits in client HTML. We keep token_plaintext
--                   alongside the hash so the project page can re-display it
--                   on every visit (the snippet needs to be copied repeatedly).
--   secret_api    — issued from the project page for /v1/* + MCP. Plaintext is
--                   shown exactly once at issuance; only the sha256 is stored.
--                   Pre-existing behaviour; the default lets existing rows
--                   land in this bucket if any ever exist.
--
-- Constraint: at most one active public token per project (enforced by a
-- partial unique index). Rotation = insert new public_ingest, revoke old, in
-- one tx. Secret tokens stay unconstrained in count.
--
-- Auth scoping (the kind ↔ surface mapping) lives in the future bearer
-- middleware, not in the schema. The schema only tracks what kind a token is.
-- Backward-compatible: add-only.

ALTER TABLE api_tokens
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'secret_api'
        CHECK (kind IN ('public_ingest', 'secret_api')),
    ADD COLUMN token_plaintext TEXT;

-- At most one active public_ingest token per project. Revoked public tokens
-- don't count (so rotation is straightforward). Secret tokens are excluded
-- from the constraint entirely.
CREATE UNIQUE INDEX api_tokens_one_active_public_per_project_idx
    ON api_tokens(project_id)
    WHERE kind = 'public_ingest' AND revoked_at IS NULL;
