-- Test restores: proving a backup restores, on a schedule.
--
-- A test restores a backup into a throwaway database (argus_test_<8 hex>)
-- on the database's own server, counts and checks it, times it, and drops
-- it. These rows are the record of each: how it went, how long the restore
-- took, and a report of numbers (never rows) the console shows. A test that
-- could not drop its copy keeps scratch_name set and scratch_dropped false,
-- and the sweep asks again until it is gone.
--
-- Plans: plans/db-backup-test-restore in the project files.

CREATE TABLE IF NOT EXISTS database_backup_tests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    backup_id       UUID NOT NULL REFERENCES database_backups(id) ON DELETE CASCADE,
    database_id     UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    -- An email, or schedule:<database id> for a scheduled test.
    started_by      TEXT NOT NULL,
    state           TEXT NOT NULL DEFAULT 'running'
                    CHECK (state IN ('running', 'passed', 'warned', 'failed', 'skipped')),
    stage           TEXT NOT NULL DEFAULT '',
    detail          TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    scratch_name    TEXT NOT NULL DEFAULT '',
    scratch_dropped BOOLEAN NOT NULL DEFAULT false,
    -- How long pg_restore took: the measured recovery time.
    restore_ms      BIGINT NOT NULL DEFAULT 0,
    tables          INT NOT NULL DEFAULT 0,
    total_rows      BIGINT NOT NULL DEFAULT 0,
    -- The restored copy's size.
    bytes           BIGINT NOT NULL DEFAULT 0,
    -- Steps, per-table row counts and each check's answer.
    report          JSONB NOT NULL DEFAULT '{}',
    started_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at     TIMESTAMPTZ,
    reported_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS database_backup_tests_db_idx ON database_backup_tests (database_id, started_at DESC);
CREATE INDEX IF NOT EXISTS database_backup_tests_backup_idx ON database_backup_tests (backup_id);
-- One running test per database; the agent runs one per server anyway.
CREATE UNIQUE INDEX IF NOT EXISTS database_backup_tests_one_running
    ON database_backup_tests (database_id) WHERE state = 'running';

-- When to test. Off for every schedule that exists already; the console
-- offers weekly when a schedule is first saved.
ALTER TABLE database_backup_schedules
    ADD COLUMN IF NOT EXISTS test_every TEXT NOT NULL DEFAULT 'off',
    ADD COLUMN IF NOT EXISTS test_hour INT NOT NULL DEFAULT 4,
    ADD COLUMN IF NOT EXISTS test_minute INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS test_weekday INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS test_next_at TIMESTAMPTZ,
    -- [{name, sql}]: an admin's checks, each one SELECT.
    ADD COLUMN IF NOT EXISTS test_checks JSONB NOT NULL DEFAULT '[]',
    -- When "no passing test for too long" was last raised, so it is raised
    -- once per stretch, not every sweep.
    ADD COLUMN IF NOT EXISTS test_overdue_at TIMESTAMPTZ;

DO $$ BEGIN
    ALTER TABLE database_backup_schedules ADD CONSTRAINT database_backup_schedules_test_every_check
        CHECK (test_every IN ('off', 'after', 'daily', 'weekly'));
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE database_backup_schedules ADD CONSTRAINT database_backup_schedules_test_hour_check
        CHECK (test_hour BETWEEN 0 AND 23);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE database_backup_schedules ADD CONSTRAINT database_backup_schedules_test_minute_check
        CHECK (test_minute BETWEEN 0 AND 59);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;
DO $$ BEGIN
    ALTER TABLE database_backup_schedules ADD CONSTRAINT database_backup_schedules_test_weekday_check
        CHECK (test_weekday BETWEEN 0 AND 6);
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Backup alerts get their own kind in the bell, next to the others, so
-- people can choose them; until now they were raised as security alerts.
ALTER TABLE console_alerts DROP CONSTRAINT IF EXISTS console_alerts_kind_check;
ALTER TABLE console_alerts ADD CONSTRAINT console_alerts_kind_check
    CHECK (kind IN ('approval', 'signin', 'session', 'security', 'server', 'backup'));
