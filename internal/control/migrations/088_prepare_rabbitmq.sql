-- Preparing RabbitMQ for the next Ubuntu release from the console: a
-- package run with mode prepare_rabbitmq upgrades it to 3.13 from
-- Canonical's PPA and enables its feature flags, which Ubuntu's upgrader
-- requires before 26.04. No new column is needed.
--
-- Only a widened CHECK: nothing here can fail on rows that exist.

ALTER TABLE package_runs DROP CONSTRAINT IF EXISTS package_runs_mode_check;
ALTER TABLE package_runs ADD CONSTRAINT package_runs_mode_check
    CHECK (mode IN ('all', 'security', 'selected', 'install', 'remove', 'script', 'file_install',
                    'app_install', 'app_uninstall', 'app_disable', 'app_restore', 'reboot', 'release_upgrade',
                    'prepare_rabbitmq'));
