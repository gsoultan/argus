-- Direct SSH logins, per login account on a server.
--
-- A server's direct SSH setting applied to every account on it, so a server
-- where staging may log in straight away and sys-admin must wait for an
-- approver could not be described. This holds a server's exceptions: account
-- name to "refuse", "approve" or "allow". An account not named follows the
-- server's setting, and that follows the deployment's.
--
-- The default is an empty object, including for every row an upgrade already
-- holds, so upgrading changes nothing until someone names an account.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS direct_ssh_accounts JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_direct_ssh_accounts_check;
ALTER TABLE assets ADD CONSTRAINT assets_direct_ssh_accounts_check
    CHECK (jsonb_typeof(direct_ssh_accounts) = 'object');
