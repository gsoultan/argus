-- Whether the console updates every agent on its own when a newer one is
-- offered: the newest Argus release it has found, or its own version.
--
-- Off until an admin turns it on. Even on, an agent updates only once
-- nobody is connected to its server, and an update to a version that went
-- back or failed on a server is not tried there again on its own.
ALTER TABLE gateway_policy ADD COLUMN IF NOT EXISTS agent_auto_update BOOLEAN NOT NULL DEFAULT FALSE;
