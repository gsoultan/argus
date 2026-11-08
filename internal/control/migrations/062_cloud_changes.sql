-- Changes to cloud resources, made from Argus.
--
-- An account that may be changed has a second role, assumed only for
-- changes (manage_role_arn). Every change is a row here that goes
-- requested -> (approved) -> applied or failed, so the audit chain shows a
-- change and its result as a pair and an applied change always has a
-- request. The plan a change was approved on is kept with its digest, and
-- a change runs only while the plan still matches.
--
-- Only added columns and a new table: nothing to rewrite on a populated
-- database.

ALTER TABLE cloud_accounts ADD COLUMN IF NOT EXISTS manage_role_arn TEXT;

CREATE TABLE IF NOT EXISTS cloud_changes (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    UUID NOT NULL REFERENCES cloud_accounts (id),
    region        TEXT NOT NULL,
    action        TEXT NOT NULL CHECK (action IN ('launch', 'start', 'stop', 'reboot', 'resize', 'tags',
                      'protect', 'unprotect', 'remove')),
    level         TEXT NOT NULL CHECK (level IN ('operate', 'change', 'remove')),
    environment   TEXT NOT NULL CHECK (environment IN ('production', 'staging', 'development')),
    -- The instance acted on; empty for a launch until it has one.
    resource_id   TEXT NOT NULL DEFAULT '',
    resource_name TEXT NOT NULL DEFAULT '',
    -- What was asked for (never the agent's enrollment token), and what it
    -- was approved on.
    spec          JSONB NOT NULL DEFAULT '{}',
    plan          JSONB NOT NULL DEFAULT '{}',
    plan_hash     TEXT NOT NULL DEFAULT '',
    summary       TEXT NOT NULL,
    reason        TEXT NOT NULL DEFAULT '',
    state         TEXT NOT NULL CHECK (state IN ('awaiting_approval', 'applying', 'applied', 'failed',
                      'declined', 'cancelled')),
    requested_by  TEXT NOT NULL,
    requested_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by    TEXT,
    decided_at    TIMESTAMPTZ,
    applied_at    TIMESTAMPTZ,
    -- The provider's request id and what the change did or why it failed.
    request_id    TEXT NOT NULL DEFAULT '',
    result        JSONB NOT NULL DEFAULT '{}',
    error         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS cloud_changes_recent ON cloud_changes (requested_at DESC);
CREATE INDEX IF NOT EXISTS cloud_changes_resource ON cloud_changes (account_id, resource_id);
-- One change in flight per instance: two people stopping and resizing the
-- same instance at once would each see a plan the other is changing.
CREATE UNIQUE INDEX IF NOT EXISTS cloud_changes_one_open ON cloud_changes (account_id, resource_id)
    WHERE state IN ('awaiting_approval', 'applying') AND resource_id <> '';
