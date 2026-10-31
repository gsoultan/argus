-- Removing a server from Argus asks its agent to uninstall itself.
--
-- Archiving the asset used to leave the agent running, still holding sshd's
-- ForceCommand: direct logins stayed refused while the console could no longer
-- open the server, so nothing reached it but its own console. The request is
-- kept on the credential, not the asset, because the agent asking is the one
-- that answers; it is read on every heartbeat until the agent revokes itself.
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS uninstall_requested_at TIMESTAMPTZ;
ALTER TABLE agent_credentials ADD COLUMN IF NOT EXISTS uninstall_requested_by TEXT;
