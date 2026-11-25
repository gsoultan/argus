-- An organization's own recording storage (org_storage.go). Without a row,
-- an organization's recordings go where the control plane's configuration
-- says, under tenants/<id>/, as before. New tables only, so an older version
-- ignores them.

-- Where the organization's recordings are copied. At most one is active;
-- a destination the organization moved away from is kept as retired, read
-- from and copied out of, and deleted once everything in it is in the new
-- place, so a recording is never stranded by a change of bucket.
CREATE TABLE IF NOT EXISTS recording_storages (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    state         TEXT NOT NULL CHECK (state IN ('active', 'retired')),
    provider      TEXT NOT NULL CHECK (provider IN ('s3', 'azure')),
    endpoint      TEXT NOT NULL DEFAULT '',
    bucket        TEXT NOT NULL CHECK (length(bucket) BETWEEN 1 AND 255),
    region        TEXT NOT NULL DEFAULT '',
    use_ssl       BOOLEAN NOT NULL DEFAULT true,
    -- The access key id, or for Azure the storage account's name.
    access_key    TEXT NOT NULL,
    -- The secret key or account key, sealed with a key derived from
    -- signing_secret and the organization; never returned.
    secret_sealed BYTEA NOT NULL,
    -- The archive's storage class or access tier in this destination.
    archive_class TEXT NOT NULL DEFAULT '',
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS recording_storages_one_active
    ON recording_storages (state) WHERE state = 'active';

-- How long the organization's servers keep their own copy, and when its
-- stored recordings are archived. 0 is the control plane's setting; -1 is
-- for ever and never. At most one row.
CREATE TABLE IF NOT EXISTS recording_policies (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    singleton     BOOLEAN NOT NULL DEFAULT true CHECK (singleton),
    keep_days     INTEGER NOT NULL DEFAULT 0 CHECK (keep_days BETWEEN -1 AND 3650),
    archive_days  INTEGER NOT NULL DEFAULT 0 CHECK (archive_days BETWEEN -1 AND 3650),
    updated_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS recording_policies_singleton ON recording_policies (singleton);
