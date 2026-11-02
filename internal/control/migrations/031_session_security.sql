-- Server-side control over console sessions, and a second factor that cannot
-- be replayed or silently replaced.
--
-- sessions_valid_after: a console cookie issued before this moment is refused.
--   Set when the password changes, the account is disabled, its second factor
--   is replaced or reset, or someone signs it out everywhere. NULL means no
--   such event yet, so every existing cookie keeps working after the upgrade.
-- totp_pending_secret: an enrolment that has not been confirmed. The secret
--   in use stays in totp_secret until a code from the new one is proved, so
--   starting an enrolment can no longer switch the second factor off.
-- totp_last_step: the last authenticator time step accepted, so each code
--   signs in once.
ALTER TABLE users
    ADD COLUMN IF NOT EXISTS sessions_valid_after TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS totp_pending_secret  TEXT,
    ADD COLUMN IF NOT EXISTS totp_last_step       BIGINT;

-- One row per signed-out cookie, kept until the cookie would have expired
-- anyway. Small: rows past expires_at are deleted as new ones arrive.
CREATE TABLE IF NOT EXISTS revoked_sessions (
    sid        TEXT        PRIMARY KEY,
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS revoked_sessions_expires_at ON revoked_sessions (expires_at);
