-- The fleet recovery key each server's agent holds, by fingerprint, from its
-- heartbeat (recovery_key.go). NULL is an agent too old to hold one, or one
-- that has not been sent it yet. Only a new column.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS agent_recovery_key TEXT;
