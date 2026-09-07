-- Local accounts: a password and a second factor, held by Argus itself.
--
-- Until now the console had no login of its own. It delegated to an OIDC
-- provider, which is right for an enterprise that already runs one and wrong
-- for everyone else: it made an identity provider a hard dependency of getting
-- into the product at all. The users table already carried `idp_subject` (null
-- for a local account) and `mfa_enrolled`, so this finishes a shape that was
-- designed for rather than adding a new one.
--
-- OIDC still works. An account may have a password, a federated identity, or
-- both, and nothing here requires a provider to exist.

-- Argon2id, in PHC string format, so the cost parameters travel with the hash
-- and can be raised later without invalidating every stored password.
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_hash TEXT;

-- The shared secret behind the six-digit codes, base32 as authenticator apps
-- expect. Present but unconfirmed means enrolment was begun and abandoned:
-- until mfa_enrolled is true this secret must not be accepted at sign-in, or
-- starting an enrolment would be enough to weaken an account.
ALTER TABLE users ADD COLUMN IF NOT EXISTS totp_secret TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS mfa_enrolled_at TIMESTAMPTZ;

-- Single-use codes for the phone that fell in a river.
--
-- Hashed, never stored in the clear: they are equivalent to the second factor,
-- so a database that leaks them has leaked MFA. One row per code rather than an
-- array, so spending one is a delete and cannot be replayed.
CREATE TABLE IF NOT EXISTS recovery_codes (
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    used_at    TIMESTAMPTZ,
    PRIMARY KEY (user_id, code_hash)
);

CREATE INDEX IF NOT EXISTS recovery_codes_user_idx ON recovery_codes (user_id) WHERE used_at IS NULL;

-- Sign-in looks accounts up by address, and does so on every attempt including
-- the failed ones a scanner produces.
CREATE INDEX IF NOT EXISTS users_email_lower_idx ON users (lower(email));

-- Disabling an account has to be possible without deleting it: the audit log
-- refers to people by address, and a deleted row makes history unreadable.
ALTER TABLE users ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;
