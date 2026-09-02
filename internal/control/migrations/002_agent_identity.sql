-- An agent identifies its host by `hostname`, which is often not the name the
-- inventory uses (a short name, a DNS alias, a cloud instance name). Without an
-- explicit mapping the heartbeat's asset update matches nothing and coverage
-- tracking silently does nothing — the failure mode is a dashboard that looks
-- correct while reporting on an empty set.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS agent_hostname TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS assets_agent_hostname_idx
    ON assets (agent_hostname) WHERE agent_hostname IS NOT NULL;

-- Heartbeats from a host that matches no asset. An agent running somewhere the
-- inventory does not know about is itself worth surfacing.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS matched_asset BOOLEAN NOT NULL DEFAULT false;
