-- The console owns the inventory, and assignment decides who reaches a host.
--
-- Until now an asset existed because someone hand-edited inventory.json on a
-- gateway, and every signed-in account could see every host and open a session
-- as any principal the file listed. That is backwards for a privileged-access
-- product: an administrator should decide what exists and who may reach it, and
-- an ordinary operator should see the hosts assigned to them and nothing else.

-- Where the row came from.
--
-- A gateway publishes its file inventory at start-up so a deployment that has
-- always been file-driven appears in the console without anyone retyping it.
-- That publish must never overwrite what an administrator entered here: the
-- upsert refuses to touch a console-owned row, and this column is what it
-- checks. 'gateway' is the default because every row that exists today got
-- there that way.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'gateway';

-- The credential the gateway injects, named rather than pathed.
--
-- A name, resolved against the gateway's own vault directory -- never a path.
-- An administrator who could type a path into this field could point the
-- gateway at any file the daemon can read and have it used as a private key,
-- which turns "add an asset" into "read anything as root". The gateway refuses
-- a separator, so the only thing this can name is a file in the vault.
--
-- Empty for ca-certificate mode, where there is no standing credential at all.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS credential_ref TEXT NOT NULL DEFAULT '';

-- The Windows domain for an RDP asset. Empty means the account is local to the
-- host, which changes the NTLM hash -- so an empty domain and the host's own
-- name are not interchangeable.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS domain TEXT NOT NULL DEFAULT '';

-- Retired, not deleted. Sessions reference assets and the audit log names them;
-- a deleted row makes the history of that host unreadable, which is precisely
-- what someone covering their tracks would reach for. An archived asset leaves
-- the inventory, the console and every gateway, and keeps its past.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS assets_live_idx ON assets (hostname) WHERE archived_at IS NULL;

-- Who may reach which host, as whom.
--
-- One row per (asset, person), holding the principals that person may assume
-- there. Deliberately explicit rather than computed from groups: the question
-- an auditor asks is "who can reach pay-01 as ops", and it should be one query
-- with one answer, not a union that has to be reconstructed.
--
-- Being assigned a principal is permission to ask, not permission to have. An
-- elevated principal still needs an approved access request on top; assignment
-- decides who may make that request at all.
CREATE TABLE IF NOT EXISTS asset_assignments (
    asset_id   UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    -- Lowercased on write. users.email is matched on lower(email) everywhere
    -- else (see 008_email_case), and an assignment that differs only in case
    -- would be an assignment nobody holds.
    user_email TEXT NOT NULL,
    principals TEXT[] NOT NULL DEFAULT '{}',
    granted_by TEXT NOT NULL DEFAULT '',
    granted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (asset_id, user_email)
);

-- Every session start asks "what is this person assigned", so the lookup by
-- person is the hot one.
CREATE INDEX IF NOT EXISTS asset_assignments_user_idx ON asset_assignments (user_email);

-- And by host, which is the console's question ("who can reach pay-01") and
-- what the cascade walks when an asset is finally deleted.
CREATE INDEX IF NOT EXISTS asset_assignments_asset_idx ON asset_assignments (asset_id);
