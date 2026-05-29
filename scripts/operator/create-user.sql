-- Creates the first user (and their personal team + membership) on a fresh
-- deployment, or any subsequent user the operator needs to bootstrap without
-- going through an invite link. Invoked from `kamal create-user` with:
--
--   psql ... -v email=<email> -v password=<new-plaintext> -f create-user.sql
--
-- Hashes the password with pgcrypto's gen_salt('bf', 10) so the produced hash
-- is wire-compatible with Go's bcrypt at cost 10 (auth.BcryptCost). The new
-- user is forced to change their password on next login via
-- must_change_password=TRUE — same gate the operator reset-password script
-- relies on.
--
-- Belt-and-suspenders: ensures pgcrypto is installed even if migration 0003
-- somehow didn't run.
--
-- psql ':var' substitution does NOT fire inside $$-quoted DO blocks, so we
-- stash the parameters as transaction-local GUCs via set_config(..., true)
-- and recover them with current_setting(...) inside the block.
--
-- ID note: the app generates UUID v7 via idgen.New() for time-sortability,
-- but the schema accepts any UUID and no codepath orders by id (ordering
-- uses created_at). To keep this script Go-free, IDs are generated here via
-- gen_random_uuid() (UUID v4). The deviation is intentional and scoped to
-- operator-created users.
--
-- Exits non-zero on validation failure or duplicate email so the kamal alias
-- surfaces operator mistakes.

\set ON_ERROR_STOP on

CREATE EXTENSION IF NOT EXISTS pgcrypto;

BEGIN;

SELECT set_config('app.create_email',    :'email',    true);
SELECT set_config('app.create_password', :'password', true);

DO $$
DECLARE
    target_email TEXT := current_setting('app.create_email', true);
    new_password TEXT := current_setting('app.create_password', true);
    new_user_id  UUID := gen_random_uuid();
    new_team_id  UUID := gen_random_uuid();
    team_name    TEXT;
BEGIN
    IF target_email IS NULL OR length(trim(target_email)) = 0 THEN
        RAISE EXCEPTION 'create-user: -v email=... is required';
    END IF;
    IF new_password IS NULL OR length(new_password) < 12 THEN
        RAISE EXCEPTION 'create-user: -v password=... must be at least 12 characters';
    END IF;

    -- Matches defaultTeamName() in internal/auth/service.go.
    team_name := split_part(target_email, '@', 1) || '''s team';

    BEGIN
        INSERT INTO users (id, email, password_hash, must_change_password)
        VALUES (
            new_user_id,
            target_email,
            crypt(new_password, gen_salt('bf', 10)),
            TRUE
        );
    EXCEPTION WHEN unique_violation THEN
        RAISE EXCEPTION 'create-user: email already exists: %', target_email;
    END;

    INSERT INTO teams (id, name) VALUES (new_team_id, team_name);

    INSERT INTO team_memberships (team_id, user_id)
    VALUES (new_team_id, new_user_id);
END
$$;

COMMIT;
