-- Cloud accounts and what Argus last read in them.
--
-- No credential is stored: an account is reached by assuming role_arn from
-- the control host's own identity, with an external id derived from the
-- root secret on every call. A disconnected account is archived, never
-- deleted, so the audit entries and changes that name it stay readable.
--
-- Only new tables: nothing to rewrite on a populated database.

CREATE TABLE IF NOT EXISTS cloud_accounts (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider      TEXT NOT NULL CHECK (provider IN ('aws')),
    name          TEXT NOT NULL,
    -- The provider's own id for the account: twelve digits on AWS.
    account_id    TEXT NOT NULL,
    role_arn      TEXT NOT NULL,
    -- Read only, until changes arrive.
    access        TEXT NOT NULL DEFAULT 'read' CHECK (access IN ('read', 'manage')),
    environment   TEXT NOT NULL DEFAULT 'production'
                  CHECK (environment IN ('production', 'staging', 'development')),
    -- Who Argus is inside the account, and the regions it has enabled,
    -- as Verify found them.
    identity_arn  TEXT NOT NULL DEFAULT '',
    regions       TEXT[] NOT NULL DEFAULT '{}',
    connected_by  TEXT NOT NULL,
    connected_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- The last sync: when it finished, and per region what failed.
    synced_at     TIMESTAMPTZ,
    sync_errors   JSONB NOT NULL DEFAULT '{}',
    archived_at   TIMESTAMPTZ,
    archived_by   TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS cloud_accounts_one_active
    ON cloud_accounts (provider, account_id) WHERE archived_at IS NULL;

-- One row per resource Argus has seen. A resource that disappears gets
-- gone_at; its row stays.
CREATE TABLE IF NOT EXISTS cloud_resources (
    account_id  UUID NOT NULL REFERENCES cloud_accounts (id),
    region      TEXT NOT NULL,
    kind        TEXT NOT NULL,
    provider_id TEXT NOT NULL,
    data        JSONB NOT NULL,
    first_seen  TIMESTAMPTZ NOT NULL DEFAULT now(),
    seen_at     TIMESTAMPTZ NOT NULL,
    gone_at     TIMESTAMPTZ,
    PRIMARY KEY (account_id, region, kind, provider_id)
);

CREATE INDEX IF NOT EXISTS cloud_resources_present ON cloud_resources (kind, account_id) WHERE gone_at IS NULL;
