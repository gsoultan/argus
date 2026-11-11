-- A release upgrade asked for from the console: a package run with mode
-- release_upgrade moves the server's operating system to its next release
-- (Ubuntu's do-release-upgrade, Debian's sources and full-upgrade, leapp on
-- RHEL and its rebuilds) and reboots into it. expect holds the release a
-- person said yes to; no new column is needed.
--
-- Only a widened CHECK: nothing here can fail on rows that exist.

ALTER TABLE package_runs DROP CONSTRAINT IF EXISTS package_runs_mode_check;
ALTER TABLE package_runs ADD CONSTRAINT package_runs_mode_check
    CHECK (mode IN ('all', 'security', 'selected', 'install', 'remove', 'script', 'file_install',
                    'app_install', 'app_uninstall', 'app_disable', 'app_restore', 'reboot', 'release_upgrade'));
