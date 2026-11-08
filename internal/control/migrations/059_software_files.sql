-- Files in the Software library, and installing them on Android devices.
--
-- A file is what Argus hands to a device or a Windows server to install: an
-- APK now, an MSI or EXE with Windows. It is stored where recordings and
-- backups are, under its SHA-256, and what it says about itself (an APK's
-- application id, version and signing certificate) is kept beside it so the
-- console shows it before anything is installed. Removing a file from the
-- library deletes the file only, never what it installed.
--
-- On a device, an install, uninstall, "disable for this user" or restore is
-- a package run (migration 050) with an app_* mode, so it travels on the
-- heartbeat, waits for approval where the update rule says so, and lands
-- in Activity like any other change.
--
-- A new table, added columns and a widened CHECK: nothing here can fail on
-- rows that exist.

CREATE TABLE IF NOT EXISTS software_files (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    platform    TEXT NOT NULL CHECK (platform IN ('android', 'windows')),
    kind        TEXT NOT NULL CHECK (kind IN ('apk', 'msi', 'exe')),
    -- What people read: the app's label, else the file's name.
    name        TEXT NOT NULL,
    file_name   TEXT NOT NULL,
    sha256      TEXT NOT NULL,
    size_bytes  BIGINT NOT NULL CHECK (size_bytes > 0),
    storage_key TEXT NOT NULL,
    -- What the file says about itself: for an APK package, version_code,
    -- version_name, signer, scheme, min_sdk.
    meta        JSONB NOT NULL DEFAULT '{}',
    note        TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    archived_at TIMESTAMPTZ
);

-- One live entry per file: the same bytes uploaded twice are the same entry.
CREATE UNIQUE INDEX IF NOT EXISTS software_files_sha256
    ON software_files (sha256) WHERE archived_at IS NULL;

ALTER TABLE package_runs DROP CONSTRAINT IF EXISTS package_runs_mode_check;
ALTER TABLE package_runs ADD CONSTRAINT package_runs_mode_check
    CHECK (mode IN ('all', 'security', 'selected', 'install', 'remove', 'script',
                    'app_install', 'app_uninstall', 'app_disable', 'app_restore'));

-- The library file an app_install installs.
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS software_file_id UUID;
