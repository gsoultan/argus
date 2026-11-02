-- How a direct login held for approval signed in: a password, or which key.
--
-- The agent learns it from sshd (ExposeAuthInfo) and checks a key against
-- the account's authorized_keys before naming it, so an approver can see
-- whose key is asking before letting it in. Empty for a row from an agent
-- older than this, or a server whose sshd does not say.
ALTER TABLE direct_login_requests
    ADD COLUMN IF NOT EXISTS auth_method     TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS key_fingerprint TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS key_type        TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS key_comment     TEXT    NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS key_found       BOOLEAN NOT NULL DEFAULT false;
