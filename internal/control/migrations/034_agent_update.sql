-- Updating an agent from the console.
--
-- The request is kept on the credential, like an uninstall: the agent that
-- holds it is the one that answers, on its heartbeat. update_id names one
-- request, so a status the agent reports can be matched to it and the
-- request cleared once it has an outcome. What the agent last said about
-- updating itself -- whether it can, and how the last one went -- is kept on
-- the agent's own row.
--
-- Columns only, all nullable or defaulted: nothing to rewrite on a
-- populated database, and an older control plane reading these tables
-- ignores them.
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS update_id TEXT;
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS update_version TEXT;
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS update_now BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS update_requested_at TIMESTAMPTZ;
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS update_requested_by TEXT;

ALTER TABLE agents ADD COLUMN IF NOT EXISTS update_status JSONB;
