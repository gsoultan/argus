-- argus-secret servers this organization uses (secrets_servers.go), and the
-- rules argus-control pushes to them. argus-control keeps no secret value and
-- no key that opens one: a row here is an address, a pin, the folders the
-- server keeps, and how far its read log has been collected. New tables, so
-- an older version ignores them.
CREATE TABLE IF NOT EXISTS secrets_servers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    -- https://host[:port], as argus-control reaches it: directly, or through
    -- Cloudflare Tunnel or another proxy.
    url             TEXT NOT NULL CHECK (url LIKE 'https://%' AND length(url) <= 300),
    -- sha256:<hex> of its certificate, or '' for a certificate the system
    -- trusts (a proxy's).
    tls_pin         TEXT NOT NULL DEFAULT '' CHECK (length(tls_pin) <= 80),
    -- Top-level folders it keeps. One server per folder in an organization,
    -- checked in secrets_servers.go under a lock on this table.
    folders         TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(folders) <= 100),
    -- What it last said about itself.
    seal_key        TEXT NOT NULL DEFAULT '',
    version         TEXT NOT NULL DEFAULT '',
    secret_count    INT NOT NULL DEFAULT 0,
    -- The rules last pushed: their serial, and a digest of their content so
    -- an unchanged bundle is not pushed again.
    rules_serial    BIGINT NOT NULL DEFAULT 0,
    rules_hash      TEXT NOT NULL DEFAULT '',
    rules_pushed_at TIMESTAMPTZ,
    -- How far its read log is in audit_events: the last seq and its link.
    log_seq         BIGINT NOT NULL DEFAULT 0,
    log_head        TEXT NOT NULL DEFAULT '',
    -- Set when its log stopped following on from what was collected: a gap
    -- or a changed entry. Collection stops until an admin resumes it.
    log_gap         TEXT NOT NULL DEFAULT '',
    log_gap_at      TIMESTAMPTZ,
    last_contact_at TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT '',
    last_error_at   TIMESTAMPTZ,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS secrets_servers_name_key ON secrets_servers (lower(name));

-- Who may do what with which secrets. Every server gets the rules for its
-- folders; argus-secret decides each read itself from them.
CREATE TABLE IF NOT EXISTS secret_rules (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- A secret name or a folder ending in '/'; '' is every secret.
    path           TEXT NOT NULL DEFAULT '' CHECK (length(path) <= 256),
    -- [{"kind","id"}]: server, group, app, person, team or role.
    subjects       JSONB NOT NULL CHECK (jsonb_typeof(subjects) = 'array'),
    can            TEXT[] NOT NULL CHECK (cardinality(can) BETWEEN 1 AND 5),
    needs_approval BOOLEAN NOT NULL DEFAULT false,
    note           TEXT NOT NULL DEFAULT '' CHECK (length(note) <= 500),
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
