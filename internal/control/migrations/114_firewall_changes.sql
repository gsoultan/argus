-- Changing the firewall on each server, and firewall policies
-- (plans/server-firewall/design.md, parts 2 to 5).
--
-- firewall_servers is what Argus keeps on each server it manages: its own
-- lines and the version of each policy it holds, as last applied. A change
-- (firewall_changes) says what to change; it is worked out against that
-- when it is sent, so changes queued behind each other do not undo each
-- other. A policy is a named, versioned set of lines put on servers by
-- name or by tag; changing it rolls out (firewall_rollouts) one server
-- first, then five at a time.
--
-- Only new tables and one added column: nothing to rewrite on a populated
-- database.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS firewall_beat JSONB;

CREATE TABLE IF NOT EXISTS firewall_servers (
    asset_id    UUID PRIMARY KEY REFERENCES assets(id) ON DELETE CASCADE,
    -- The server's own lines, with their sources in words (servers, tags,
    -- policies, networks), as last applied.
    own_lines   JSONB NOT NULL DEFAULT '[]',
    -- Policy id to the version the server holds.
    policies    JSONB NOT NULL DEFAULT '{}',
    -- What was last sent for traffic going out that no line names.
    default_out TEXT NOT NULL DEFAULT '' CHECK (default_out IN ('', 'allow', 'deny')),
    -- Operators who may change this server's firewall, by email.
    managers    TEXT[] NOT NULL DEFAULT '{}',
    -- When Argus last made the server match what it keeps.
    synced_at   TIMESTAMPTZ,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS firewall_policies (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 80),
    description TEXT NOT NULL DEFAULT '',
    version     INTEGER NOT NULL DEFAULT 1,
    lines       JSONB NOT NULL DEFAULT '[]',
    -- Going out: allow everything (as AWS does) or only the lines.
    default_out TEXT NOT NULL DEFAULT 'allow' CHECK (default_out IN ('allow', 'deny')),
    -- Came with Argus.
    starter     BOOLEAN NOT NULL DEFAULT false,
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by  TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS firewall_policies_name ON firewall_policies (lower(name));

-- Every version of a policy, frozen.
CREATE TABLE IF NOT EXISTS firewall_policy_versions (
    policy_id   UUID NOT NULL REFERENCES firewall_policies(id) ON DELETE CASCADE,
    version     INTEGER NOT NULL,
    lines       JSONB NOT NULL,
    default_out TEXT NOT NULL CHECK (default_out IN ('allow', 'deny')),
    note        TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (policy_id, version)
);

-- Where a policy is: a server, or every server with a tag.
CREATE TABLE IF NOT EXISTS firewall_policy_targets (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_id  UUID NOT NULL REFERENCES firewall_policies(id) ON DELETE CASCADE,
    asset_id   UUID REFERENCES assets(id) ON DELETE CASCADE,
    tag        TEXT,
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK ((asset_id IS NULL) <> (tag IS NULL))
);
CREATE UNIQUE INDEX IF NOT EXISTS firewall_policy_targets_asset ON firewall_policy_targets (policy_id, asset_id) WHERE asset_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS firewall_policy_targets_tag ON firewall_policy_targets (policy_id, lower(tag)) WHERE tag IS NOT NULL;

-- A policy's new version, or where it is, going out to its servers.
CREATE TABLE IF NOT EXISTS firewall_rollouts (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    policy_id    UUID REFERENCES firewall_policies(id) ON DELETE SET NULL,
    policy_name  TEXT NOT NULL,
    version      INTEGER NOT NULL,
    state        TEXT NOT NULL CHECK (state IN ('awaiting_approval', 'running', 'done', 'stopped', 'denied', 'cancelled')),
    summary      TEXT NOT NULL DEFAULT '',
    requested_by TEXT NOT NULL,
    requested_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by   TEXT,
    decided_at   TIMESTAMPTZ,
    ended_at     TIMESTAMPTZ,
    message      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS firewall_rollouts_policy ON firewall_rollouts (policy_id, requested_at DESC);

CREATE TABLE IF NOT EXISTS firewall_changes (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id       UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    -- The name the agent reports under, and the server's own name.
    hostname       TEXT NOT NULL,
    asset_hostname TEXT NOT NULL,
    kind           TEXT NOT NULL CHECK (kind IN ('lines', 'take_over', 'turn_on', 'put_back', 'policy', 'sync')),
    summary        TEXT NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    -- What to change, against what the server holds when it is sent.
    delta          JSONB NOT NULL,
    -- What the server held before (for Put back) and what was sent.
    before         JSONB,
    spec           JSONB,
    -- The commands the review screen showed.
    preview        JSONB NOT NULL DEFAULT '[]',
    state          TEXT NOT NULL CHECK (state IN ('awaiting_approval', 'queued', 'sent', 'trial', 'waiting_keep',
                       'applied', 'failed', 'rolled_back', 'denied', 'cancelled', 'interrupted', 'stopped')),
    rollout_id     UUID REFERENCES firewall_rollouts(id) ON DELETE SET NULL,
    wave           INTEGER NOT NULL DEFAULT 0,
    requested_by   TEXT NOT NULL,
    requested_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_by     TEXT,
    decided_at     TIMESTAMPTZ,
    kept_by        TEXT,
    kept_at        TIMESTAMPTZ,
    sent_at        TIMESTAMPTZ,
    ended_at       TIMESTAMPTZ,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    message        TEXT NOT NULL DEFAULT '',
    -- Each command the agent ran, and how it ended.
    ran            JSONB NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS firewall_changes_asset ON firewall_changes (asset_id, requested_at DESC);
CREATE INDEX IF NOT EXISTS firewall_changes_host ON firewall_changes (lower(hostname)) WHERE state IN ('queued', 'sent', 'trial', 'waiting_keep');
CREATE INDEX IF NOT EXISTS firewall_changes_rollout ON firewall_changes (rollout_id, wave) WHERE rollout_id IS NOT NULL;
