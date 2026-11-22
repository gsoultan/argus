-- WireGuard networks between servers, set up through their agents
-- (wg_networks.go). New tables and a new agents column, so an older version
-- ignores all three.
CREATE TABLE IF NOT EXISTS wg_networks (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    -- mesh: every member reaches every other directly. hub: members reach
    -- each other through the one hub.
    shape         TEXT NOT NULL CHECK (shape IN ('mesh', 'hub')),
    cidr          TEXT NOT NULL CHECK (length(cidr) <= 18),
    port          INTEGER NOT NULL CHECK (port BETWEEN 1024 AND 65535),
    -- The interface on every member: argus and six hex digits.
    iface         TEXT NOT NULL CHECK (iface ~ '^argus[0-9a-f]{6}$'),
    -- waiting until a second admin approves it; nothing reaches a server
    -- before then. active once approved.
    state         TEXT NOT NULL DEFAULT 'waiting' CHECK (state IN ('waiting', 'active')),
    -- A change waiting for approval (WGNetworkDraft): the whole network as
    -- it would be, or {"remove": true}.
    pending       JSONB CHECK (pending IS NULL OR octet_length(pending::text) <= 100000),
    pending_by    TEXT NOT NULL DEFAULT '',
    pending_at    TIMESTAMPTZ,
    requested_by  TEXT NOT NULL,
    approved_by   TEXT NOT NULL DEFAULT '',
    approved_at   TIMESTAMPTZ,
    generation    INTEGER NOT NULL DEFAULT 0,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_wg_networks_name UNIQUE (name),
    CONSTRAINT uq_wg_networks_iface UNIQUE (iface)
);

-- An approved member. Its public key is what its agent reported; the
-- private key never leaves the server.
CREATE TABLE IF NOT EXISTS wg_members (
    network_id  UUID NOT NULL REFERENCES wg_networks(id) ON DELETE CASCADE,
    asset_id    UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    address     TEXT NOT NULL CHECK (length(address) <= 15),
    hub         BOOLEAN NOT NULL DEFAULT false,
    -- Where the others reach it, without the port; '' when it only dials out.
    endpoint    TEXT NOT NULL DEFAULT '' CHECK (length(endpoint) <= 253),
    public_key  TEXT NOT NULL DEFAULT '' CHECK (length(public_key) <= 44),
    key_at      TIMESTAMPTZ,
    PRIMARY KEY (network_id, asset_id)
);
CREATE INDEX IF NOT EXISTS wg_members_asset_id_idx ON wg_members (asset_id);

-- What the agent last said: whether it can, and how its last change went.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS wgnet_state JSONB
    CHECK (wgnet_state IS NULL OR octet_length(wgnet_state::text) <= 20000);
