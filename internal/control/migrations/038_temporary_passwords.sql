-- Temporary passwords, and when a server first connected.
--
-- A person added from the console is given a password nobody chose: emailed to
-- them, or shown once to the administrator who added them. It is temporary in
-- two ways. password_change_required holds the account to "change your
-- password" until they set one of their own, so the password the
-- administrator could have seen never becomes the one in use; and
-- password_expires_at stops it working at all if it is not used in time, so
-- an invitation sitting in a mailbox is not a credential for ever. Setting
-- your own password clears both.
--
-- Defaults keep every existing account as it is: false, and no expiry.
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_change_required BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE users ADD COLUMN IF NOT EXISTS password_expires_at TIMESTAMPTZ;

-- first_connected_at is when the agent holding this credential first opened
-- its tunnel, so "server connected" can be told once rather than on every
-- reconnect. Every credential that exists before this migration is taken to
-- have connected already: an upgrade must not announce the whole fleet.
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS first_connected_at TIMESTAMPTZ;
UPDATE agent_credentials
   SET first_connected_at = COALESCE(tunnel_connected_at, created_at)
 WHERE first_connected_at IS NULL;
