-- IPsec links from a server to a cloud VPN gateway or an office firewall
-- (ipsec_links.go), set up with strongSwan through the server's agent. A new
-- table, so an older version ignores it.
CREATE TABLE IF NOT EXISTS ipsec_links (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    -- The swanctl connection and file name: argus- and eight hex digits.
    conn          TEXT NOT NULL CHECK (conn ~ '^argus-[0-9a-f]{8}$'),
    -- The Argus server that is this side's gateway.
    asset_id      UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    peer          TEXT NOT NULL CHECK (length(peer) BETWEEN 1 AND 253),
    local_ranges  TEXT[] NOT NULL CHECK (cardinality(local_ranges) BETWEEN 1 AND 16),
    remote_ranges TEXT[] NOT NULL CHECK (cardinality(remote_ranges) BETWEEN 1 AND 16),
    proposals     TEXT NOT NULL CHECK (proposals IN ('strong', 'compatible')),
    -- The pre-shared key, sealed with a key derived from signing_secret;
    -- never returned except to an admin who asks, which is audited.
    psk_sealed    BYTEA NOT NULL,
    -- waiting until a second admin approves it; active once approved.
    state         TEXT NOT NULL DEFAULT 'waiting' CHECK (state IN ('waiting', 'active')),
    -- A change waiting for approval (IPsecDraft), or {"remove": true}.
    pending       JSONB CHECK (pending IS NULL OR octet_length(pending::text) <= 20000),
    pending_by    TEXT NOT NULL DEFAULT '',
    pending_at    TIMESTAMPTZ,
    requested_by  TEXT NOT NULL,
    approved_by   TEXT NOT NULL DEFAULT '',
    approved_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_ipsec_links_name UNIQUE (name),
    CONSTRAINT uq_ipsec_links_conn UNIQUE (conn)
);
CREATE INDEX IF NOT EXISTS ipsec_links_asset_id_idx ON ipsec_links (asset_id);
