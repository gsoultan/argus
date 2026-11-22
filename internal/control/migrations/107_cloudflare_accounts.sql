-- Cloudflare accounts read for Networks (cloudflare_accounts.go): tunnels,
-- WARP Connectors, private network routes, WARP devices and Magic WAN, with
-- a read-only API token. A new table and a new alert kind, so an older
-- version ignores both.
CREATE TABLE IF NOT EXISTS cloudflare_accounts (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    -- Cloudflare's own account id.
    account_id    TEXT NOT NULL CHECK (account_id ~ '^[0-9a-f]{32}$'),
    -- The API token, sealed with a key derived from signing_secret; never
    -- returned. token_tail is its last three characters.
    token_sealed  BYTEA NOT NULL,
    token_tail    TEXT NOT NULL DEFAULT '',
    -- The last read (cfapi.Snapshot), bounded.
    snapshot      JSONB CHECK (snapshot IS NULL OR octet_length(snapshot::text) <= 2000000),
    synced_at     TIMESTAMPTZ,
    last_error    TEXT NOT NULL DEFAULT '',
    last_error_at TIMESTAMPTZ,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT uq_cloudflare_accounts_account_id UNIQUE (account_id)
);

-- A VPN link, cloud VPN or Cloudflare tunnel going down, and coming back.
ALTER TABLE console_alerts DROP CONSTRAINT IF EXISTS console_alerts_kind_check;
ALTER TABLE console_alerts ADD CONSTRAINT console_alerts_kind_check
    CHECK (kind IN ('approval', 'signin', 'session', 'security', 'server', 'backup', 'data', 'updates', 'resources', 'cloud', 'website', 'network'));
