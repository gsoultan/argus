-- The lag watch on streaming replicas of a managed Postgres (cloud scaling
-- step 8c): what the primary last said about each replica that is done.
ALTER TABLE database_replicas
    ADD COLUMN IF NOT EXISTS streaming      BOOLEAN NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS lag_s          DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS lag_bytes      BIGINT,
    ADD COLUMN IF NOT EXISTS retained_bytes BIGINT,
    ADD COLUMN IF NOT EXISTS watched_at     TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS watch_error    TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS behind_since   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS behind_alerted BOOLEAN NOT NULL DEFAULT false;
