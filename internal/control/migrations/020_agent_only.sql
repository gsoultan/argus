-- Agent only, with no switch.
--
-- Every session opens from the Argus console and reaches the server through
-- its agent. The switch that let ssh, scp, sftp and mstsc in without the
-- console is gone, so its column goes too: a column nothing reads would only
-- suggest a setting that no longer exists.
--
-- assets.agent_console_only stays. Agents still report whether they are
-- refusing direct logins, and the console still shows it.
ALTER TABLE gateway_policy DROP COLUMN IF EXISTS allow_native_clients;
