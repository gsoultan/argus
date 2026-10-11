-- Whether an enrolled agent's tunnel is up, and how well.
--
-- Each agent holds one outbound connection to a control plane replica. Any
-- replica can be asked about it, so the state lives here rather than in the
-- memory of the one holding it. tunnel_seen_at is refreshed by that replica
-- every ping; a row whose seen time is old belongs to a replica that went away
-- without saying so, and the console shows it as down, not as connected.
--
-- All nullable, all empty for every existing row: nothing is connected until
-- an agent connects.
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS tunnel_transport    TEXT;
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS tunnel_replica      TEXT;
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS tunnel_connected_at TIMESTAMPTZ;
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS tunnel_seen_at      TIMESTAMPTZ;
-- Microseconds, measured end to end through the tunnel. The console's speed
-- claims have to be checkable against something.
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS tunnel_rtt_us       INTEGER;
