-- The control server's own helper: argus-control-host, the agent shipped in
-- the argus-control package and enrolled by it, so the console can clean the
-- control server's logs, update its packages, grow its disks and open a
-- terminal on it without anyone installing an agent there. See
-- control_host.go.
--
-- A token minted by argus-control for its own host says so, the credential
-- it is exchanged for carries it on, and the server that enrollment adds is
-- marked: such a server updates with argus-control rather than from the
-- console, and is not removed from the inventory, because the package would
-- only enroll it again.
ALTER TABLE agent_enrollment_tokens ADD COLUMN IF NOT EXISTS control_host BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS control_host BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE assets ADD COLUMN IF NOT EXISTS control_host BOOLEAN NOT NULL DEFAULT false;
