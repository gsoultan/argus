-- Which logins the agent on a host records.
--
-- 'every_login' is what the agent has always done: every sshd login goes
-- through the shim and is recorded, whether or not it came through Argus.
-- 'argus_only' records sessions that arrive through Argus and lets a direct
-- login through unrecorded -- but never unseen: the agent still reports it,
-- and the control plane writes it into the audit chain as a bypass.
--
-- The default is the strict one, and it is also what every existing row means
-- today, so the upgrade changes nothing about what a fleet records.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS recording_scope TEXT NOT NULL DEFAULT 'every_login';

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'assets_recording_scope_check') THEN
        ALTER TABLE assets ADD CONSTRAINT assets_recording_scope_check
            CHECK (recording_scope IN ('every_login', 'argus_only'));
    END IF;
END $$;

-- What the agent says it is applying, from its last heartbeat.
--
-- A setting the console shows as saved is not a setting the host obeys until
-- the agent has picked it up. NULL means the agent has not said -- an agent
-- older than this column, or one that has not reported since -- and the console
-- must show that as unconfirmed, never as applied.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS agent_recording_scope TEXT;
