-- Which logins are recorded, removed.
--
-- recording_scope let a direct sshd login through unrecorded ('argus_only').
-- Every deployment is now console-only: the agent refuses a direct login
-- before the scope was ever consulted, so the setting decided nothing. Its
-- columns go with it, so the schema does not suggest a choice that is not
-- there. Every session is recorded.
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_recording_scope_check;
ALTER TABLE assets DROP COLUMN IF EXISTS recording_scope;
ALTER TABLE assets DROP COLUMN IF EXISTS agent_recording_scope;
