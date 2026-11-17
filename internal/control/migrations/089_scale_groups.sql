-- Scale groups: servers that do the same job behind one load balancer, with
-- a rule for when to add one and when to take one away (scale_groups.go,
-- scaler.go). Only new tables, and two wider checks on cloud_changes.

CREATE TABLE IF NOT EXISTS scale_groups (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id      UUID NOT NULL REFERENCES cloud_accounts (id) ON DELETE CASCADE,
    region          TEXT NOT NULL,
    name            TEXT NOT NULL CHECK (name ~ '^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$'),
    environment     TEXT NOT NULL CHECK (environment IN ('production', 'staging', 'development')),
    -- Where traffic comes in: the target group the members are in, its
    -- port, and the load balancer in front of it, by name.
    target_group    TEXT NOT NULL,
    target_name     TEXT NOT NULL DEFAULT '',
    port            INTEGER NOT NULL DEFAULT 0,
    balancer        TEXT NOT NULL DEFAULT '',
    -- What a new member is launched from: image, size, disk, subnets,
    -- firewalls and tags (scaleTemplate).
    template        JSONB NOT NULL,
    min_size        INTEGER NOT NULL CHECK (min_size >= 1),
    max_size        INTEGER NOT NULL CHECK (max_size >= min_size),
    cost_cap        DOUBLE PRECISION NOT NULL DEFAULT 0,
    add_wait        INTEGER NOT NULL DEFAULT 10,
    remove_wait     INTEGER NOT NULL DEFAULT 30,
    rules           JSONB NOT NULL,
    mode            TEXT NOT NULL DEFAULT 'ask' CHECK (mode IN ('ask', 'auto')),
    -- Who turned automatic on, and when: every automatic action names them.
    auto_by         TEXT NOT NULL DEFAULT '',
    auto_at         TIMESTAMPTZ,
    state           TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'paused')),
    paused_reason   TEXT NOT NULL DEFAULT '',
    -- What the rules said the last time Argus looked, in words.
    note            TEXT NOT NULL DEFAULT '',
    -- The address new servers reach Argus at, as the person who made the
    -- group saw it, for the agent's first boot.
    enroll_base     TEXT NOT NULL DEFAULT '',
    failed_starts   INTEGER NOT NULL DEFAULT 0,
    last_add_at     TIMESTAMPTZ,
    last_remove_at  TIMESTAMPTZ,
    checked_at      TIMESTAMPTZ,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS scale_groups_name ON scale_groups (account_id, region, name);

CREATE TABLE IF NOT EXISTS scale_group_members (
    group_id       UUID NOT NULL REFERENCES scale_groups (id) ON DELETE CASCADE,
    instance_id    TEXT NOT NULL,
    hostname       TEXT NOT NULL,
    zone           TEXT NOT NULL DEFAULT '',
    subnet         TEXT NOT NULL DEFAULT '',
    -- joining: launched, not yet in the target group; member: in it;
    -- draining: taken out, terminated at drain_until; gone: terminated;
    -- failed: never became healthy.
    state          TEXT NOT NULL CHECK (state IN ('joining', 'member', 'draining', 'gone', 'failed')),
    added_by       TEXT NOT NULL,
    joined_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    registered_at  TIMESTAMPTZ,
    drain_until    TIMESTAMPTZ,
    left_at        TIMESTAMPTZ,
    PRIMARY KEY (group_id, instance_id)
);

CREATE TABLE IF NOT EXISTS scale_group_activity (
    id         BIGSERIAL PRIMARY KEY,
    group_id   UUID NOT NULL REFERENCES scale_groups (id) ON DELETE CASCADE,
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- created, changed, rule, asked, lapsed, added, joined, removed,
    -- drained, failed, paused, resumed.
    kind       TEXT NOT NULL,
    actor      TEXT NOT NULL,
    detail     TEXT NOT NULL,
    change_id  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS scale_group_activity_recent ON scale_group_activity (group_id, at DESC);

ALTER TABLE cloud_changes DROP CONSTRAINT IF EXISTS cloud_changes_action_check;
ALTER TABLE cloud_changes ADD CONSTRAINT cloud_changes_action_check CHECK (action IN (
    'launch', 'start', 'stop', 'hibernate', 'reboot', 'resize', 'tags', 'protect', 'unprotect', 'settings', 'remove',
    'volume-create', 'volume-grow', 'volume-attach', 'volume-detach', 'volume-delete',
    'snapshot-create', 'snapshot-delete', 'image-create', 'image-delete',
    'address-allocate', 'address-associate', 'address-disassociate', 'address-release',
    'firewall-create', 'firewall-rules', 'firewall-delete', 'instance-firewalls',
    'target-register', 'target-deregister', 'capacity-create', 'capacity-cancel', 'ri-buy', 'sp-buy',
    'target-group-create', 'health-check', 'balancer-create', 'group-size',
    'scale-group-create', 'scale-group-change', 'scale-add', 'scale-remove'));

-- An "Ask each time" question nobody answered in 30 minutes lapses.
ALTER TABLE cloud_changes DROP CONSTRAINT IF EXISTS cloud_changes_state_check;
ALTER TABLE cloud_changes ADD CONSTRAINT cloud_changes_state_check CHECK (state IN (
    'awaiting_approval', 'applying', 'applied', 'failed', 'declined', 'cancelled', 'lapsed'));
