-- What the agent has been trying to say for a week.
--
-- The agent sends exec_tracing and exec_reason with every heartbeat: whether
-- its kernel probe is loaded, and why not when it is not. The heartbeat handler
-- had no such fields, decode() rejects unknown fields by design, and so every
-- heartbeat from every agent came back 400 -- not the two fields, the whole
-- message.
--
-- Nothing about that was visible. The agent spools and retries, the control
-- plane logs a bad request among many, and the only symptom is that agents
-- drift to 'stale' and stay there. On this dev fleet the last successful
-- heartbeat was seven days before the field was added.
--
-- Everything a heartbeat carries was lost with it: active session counts,
-- posture, drift, and the probe state that "require eBPF for root" needs in
-- order to be a control rather than a switch.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS exec_tracing BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS exec_reason  TEXT    NOT NULL DEFAULT '';

-- Answered per session-open on an elevated principal, so it wants an index.
CREATE INDEX IF NOT EXISTS agents_exec_tracing_idx ON agents (hostname) WHERE exec_tracing;
