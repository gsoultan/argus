-- Restarting an agent from the console.
--
-- A restart rides the same request as an update (034): one request per
-- credential, named by update_id, cleared once the agent reports how it
-- went. update_restart says the request is a restart on the version the
-- agent already runs, rather than an install of update_version.
--
-- One defaulted column: nothing to rewrite on a populated database, and an
-- older control plane reading the table ignores it.
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS update_restart BOOLEAN NOT NULL DEFAULT false;
