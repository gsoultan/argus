-- Streaming replicas of a Postgres on a server Argus manages, onto another
-- server Argus manages (cloud scaling step 8b). One row per replica: asked,
-- approved, copied, added to Argus as its own database.
CREATE TABLE IF NOT EXISTS database_replicas (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    database_id         UUID NOT NULL REFERENCES databases(id) ON DELETE CASCADE,
    asset_id            UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    name                TEXT NOT NULL CHECK (name ~ '^[a-z][a-z0-9_]{0,39}$'),
    -- The replica's address as the primary sees it, and the primary's as
    -- the replica reaches it.
    replica_address     TEXT NOT NULL,
    primary_address     TEXT NOT NULL,
    requested_by        TEXT NOT NULL,
    reason              TEXT NOT NULL DEFAULT '',
    request_id          UUID REFERENCES access_requests(id) ON DELETE SET NULL,
    state               TEXT NOT NULL DEFAULT 'pending'
                        CHECK (state IN ('pending', 'denied', 'cancelled', 'running', 'done', 'failed')),
    stage               TEXT NOT NULL DEFAULT '',
    detail              TEXT NOT NULL DEFAULT '',
    error               TEXT NOT NULL DEFAULT '',
    replica_database_id UUID REFERENCES databases(id) ON DELETE SET NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at          TIMESTAMPTZ,
    finished_at         TIMESTAMPTZ,
    UNIQUE (database_id, name)
);
CREATE INDEX IF NOT EXISTS database_replicas_asset_idx ON database_replicas (asset_id);
CREATE INDEX IF NOT EXISTS database_replicas_request_idx ON database_replicas (request_id);
