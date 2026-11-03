-- What kind of machine each server is, and how busy it is.
--
-- system: kernel, architecture, processor, memory size, how it is hosted,
--   when it booted and its time zone. Rides the host facts report, so it
--   changes when the machine does.
-- usage, usage_at: load, processor, memory, swap and how full each disk is,
--   as of usage_at (the agent's sample time, never later than when it
--   arrived). Rides about one heartbeat a minute.
--
-- All NULL until an agent new enough to report them does, so the console can
-- say "not reported" rather than show zeroes. Read-only facts any local user
-- could see; nothing here is a secret.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS system   JSONB;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS usage    JSONB;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS usage_at TIMESTAMPTZ;
