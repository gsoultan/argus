-- Installing on Windows servers from the software library: a package run
-- with mode file_install carries an MSI or EXE (software_files, migration
-- 059) by its id, like an app_install does an APK.
--
-- Only a widened CHECK: nothing here can fail on rows that exist.

ALTER TABLE package_runs DROP CONSTRAINT IF EXISTS package_runs_mode_check;
ALTER TABLE package_runs ADD CONSTRAINT package_runs_mode_check
    CHECK (mode IN ('all', 'security', 'selected', 'install', 'remove', 'script', 'file_install',
                    'app_install', 'app_uninstall', 'app_disable', 'app_restore'));
