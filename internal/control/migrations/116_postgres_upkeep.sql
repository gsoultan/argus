-- PostgreSQL and apt sources from a server's Updates tab: a package run with
-- mode postgres_upgrade moves a cluster to a newer major version (expect is
-- "17/main:19"), postgres_drop_old drops the stopped cluster an upgrade left
-- (expect is "17/main"), and apt_sources_fix moves aside files apt ignores
-- and points apt.postgresql.org at the server's release (expect is the
-- fingerprint of what the console showed). No new column is needed.
--
-- Only a widened CHECK: nothing here can fail on rows that exist.

ALTER TABLE package_runs DROP CONSTRAINT IF EXISTS package_runs_mode_check;
ALTER TABLE package_runs ADD CONSTRAINT package_runs_mode_check
    CHECK (mode IN ('all', 'security', 'selected', 'install', 'remove', 'script', 'file_install',
                    'app_install', 'app_uninstall', 'app_disable', 'app_restore', 'reboot', 'release_upgrade',
                    'prepare_rabbitmq', 'postgres_upgrade', 'postgres_drop_old', 'apt_sources_fix'));
