-- Console-only access.
--
-- With allow_native_clients false, every session opens from the Argus console:
-- the gateway refuses ssh, scp, sftp and mstsc on its own listeners, and an
-- agent refuses a login that reached sshd without going through Argus. True
-- puts back what a deployment allowed before this column existed.
--
-- The default is the closed position, on purpose, and that includes the row an
-- upgrade already holds: console-only was asked for as the product's rule, not
-- as an option to discover. An owner who still needs native clients turns them
-- back on in Settings, and that change is audited as a loosening.
ALTER TABLE gateway_policy ADD COLUMN IF NOT EXISTS allow_native_clients BOOLEAN NOT NULL DEFAULT FALSE;

-- Whether the agent on a host says it is refusing direct logins, from its last
-- heartbeat. NULL is "has not said" -- an agent older than this column, or one
-- that has not reported since -- and the console shows it as unconfirmed.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS agent_console_only BOOLEAN;
