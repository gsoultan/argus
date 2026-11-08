-- A reboot asked for from the console: a package run with mode reboot
-- installs nothing and reboots the server, for one waiting on a reboot
-- after its updates.
--
-- Only a widened CHECK: nothing here can fail on rows that exist.

ALTER TABLE package_runs DROP CONSTRAINT IF EXISTS package_runs_mode_check;
ALTER TABLE package_runs ADD CONSTRAINT package_runs_mode_check
    CHECK (mode IN ('all', 'security', 'selected', 'install', 'remove', 'script', 'file_install',
                    'app_install', 'app_uninstall', 'app_disable', 'app_restore', 'reboot'));
