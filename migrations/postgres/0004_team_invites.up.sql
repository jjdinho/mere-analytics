-- Team invites: a one-shot link the inviter shares out-of-band. The plaintext
-- token lives in the URL; only sha256(token) is stored, mirroring api_tokens.
-- Consumed by a single atomic UPDATE ... RETURNING in auth.Service so two
-- concurrent claimants can never both succeed.
-- Backward-compatible: add-only (per TODOS.md convention).

CREATE TABLE team_invites (
    id            UUID PRIMARY KEY,
    team_id       UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    created_by    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash    TEXT NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    consumed_at   TIMESTAMPTZ,
    consumed_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Hot path: consume-by-hash for unconsumed invites. UNIQUE on the active set
-- doubles as collision insurance (32 random bytes → 2^256, but the index is
-- free to add). Plan §"Decisions for this step" Issue 16.
CREATE UNIQUE INDEX team_invites_token_hash_active_idx
    ON team_invites(token_hash)
    WHERE consumed_at IS NULL;

-- Listing pending invites on a team's settings page.
CREATE INDEX team_invites_team_id_idx
    ON team_invites(team_id)
    WHERE consumed_at IS NULL;
