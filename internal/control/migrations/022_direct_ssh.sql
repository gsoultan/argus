-- Direct SSH logins: refused, held for approval, or allowed and recorded.
--
-- Until now every deployment refused a login that reached sshd without the
-- console, with no setting. This puts the choice back, deliberately narrower
-- than the old allow_native_clients: whatever is chosen, an admitted login is
-- recorded, and the only way past "refuse" without a person deciding is a mode
-- an admin picked and the audit log names.
--
-- The default is 'refuse', including for the row an upgrade already holds, so
-- upgrading changes nothing until someone chooses otherwise.
ALTER TABLE gateway_policy ADD COLUMN IF NOT EXISTS direct_ssh TEXT NOT NULL DEFAULT 'refuse';
ALTER TABLE gateway_policy DROP CONSTRAINT IF EXISTS gateway_policy_direct_ssh_check;
ALTER TABLE gateway_policy ADD CONSTRAINT gateway_policy_direct_ssh_check
    CHECK (direct_ssh IN ('refuse', 'approve', 'allow'));

-- A server's own choice. NULL follows the deployment's.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS direct_ssh TEXT;
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_direct_ssh_check;
ALTER TABLE assets ADD CONSTRAINT assets_direct_ssh_check
    CHECK (direct_ssh IS NULL OR direct_ssh IN ('refuse', 'approve', 'allow'));

-- What the agent says it is applying, from its last heartbeat. NULL is an
-- agent too old to say, shown as unconfirmed.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS agent_direct_ssh TEXT;

-- One row per direct login held for approval. The agent creates it after sshd
-- has authenticated the account, so rows only come from someone who already
-- holds a working key or password; the routes still cap how many one host can
-- have pending.
CREATE TABLE IF NOT EXISTS direct_login_requests (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    hostname         TEXT NOT NULL,
    principal        TEXT NOT NULL,
    client_addr      TEXT NOT NULL DEFAULT '',
    command          TEXT NOT NULL DEFAULT '',
    state            TEXT NOT NULL DEFAULT 'pending'
                     CHECK (state IN ('pending', 'approved', 'denied', 'expired')),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at       TIMESTAMPTZ NOT NULL,
    decided_at       TIMESTAMPTZ,
    decided_by_email TEXT,
    decision_note    TEXT,
    CHECK ((state = 'pending') = (decided_at IS NULL))
);
CREATE INDEX IF NOT EXISTS direct_login_requests_created_idx
    ON direct_login_requests (created_at DESC);
CREATE INDEX IF NOT EXISTS direct_login_requests_pending_idx
    ON direct_login_requests (lower(hostname)) WHERE state = 'pending';
