-- A read endpoint for a managed Postgres (cloud scaling step 8c-2): an NLB
-- target group Argus keeps to the primary and its replicas that are not
-- behind. The instance ids are what Argus put in, so it takes out exactly
-- that, even after a server is matched to another instance.
CREATE TABLE IF NOT EXISTS database_read_endpoints (
    database_id      UUID PRIMARY KEY REFERENCES databases(id) ON DELETE CASCADE,
    account_id       UUID NOT NULL REFERENCES cloud_accounts(id) ON DELETE CASCADE,
    region           TEXT NOT NULL,
    target_group     TEXT NOT NULL,
    target_name      TEXT NOT NULL DEFAULT '',
    include_primary  BOOLEAN NOT NULL DEFAULT true,
    primary_instance TEXT NOT NULL DEFAULT '',
    note             TEXT NOT NULL DEFAULT '',
    set_by           TEXT NOT NULL,
    set_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS database_read_endpoints_account_idx ON database_read_endpoints (account_id);

ALTER TABLE database_replicas
    ADD COLUMN IF NOT EXISTS endpoint_instance TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS endpoint_note     TEXT NOT NULL DEFAULT '';
