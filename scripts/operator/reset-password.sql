-- Resets a user's password to a new plaintext value. Invoked from
-- `kamal reset-password` (defined in config/deploy.yml in step 10) with:
--
--   psql ... -v email=<email> -v password=<new-plaintext> -f reset-password.sql
--
-- Hashes with pgcrypto's gen_salt('bf', 10) so the produced hash is
-- wire-compatible with Go's bcrypt at cost 10 (auth.BcryptCost). The user is
-- forced to change their password on next login via must_change_password.
--
-- Belt-and-suspenders: ensures pgcrypto is installed even if migration 0003
-- somehow didn't run.
--
-- psql ':var' substitution does NOT fire inside $$-quoted DO blocks, so we
-- stash the parameters as transaction-local GUCs via set_config(..., true)
-- and recover them with current_setting(...) inside the block.
--
-- Exits non-zero on unknown email so the kamal alias surfaces typos.

\set ON_ERROR_STOP on

CREATE EXTENSION IF NOT EXISTS pgcrypto;

BEGIN;

SELECT set_config('app.reset_email',    :'email',    true);
SELECT set_config('app.reset_password', :'password', true);

DO $$
DECLARE
    target_email TEXT := current_setting('app.reset_email', true);
    new_password TEXT := current_setting('app.reset_password', true);
    affected     INTEGER;
BEGIN
    IF target_email IS NULL OR length(trim(target_email)) = 0 THEN
        RAISE EXCEPTION 'reset-password: -v email=... is required';
    END IF;
    IF new_password IS NULL OR length(new_password) < 12 THEN
        RAISE EXCEPTION 'reset-password: -v password=... must be at least 12 characters';
    END IF;

    UPDATE users
    SET password_hash        = crypt(new_password, gen_salt('bf', 10)),
        must_change_password = TRUE,
        updated_at           = NOW()
    WHERE lower(email) = lower(target_email);

    GET DIAGNOSTICS affected = ROW_COUNT;
    IF affected = 0 THEN
        RAISE EXCEPTION 'reset-password: no user with email %', target_email;
    END IF;
END
$$;

COMMIT;
