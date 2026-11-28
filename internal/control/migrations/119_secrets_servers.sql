-- argus-secret servers this organization uses (secrets_servers.go), and the
-- rules argus-control pushes to them. argus-control keeps no secret value and
-- no key that opens one: a row here is an address, a pin, the folders the
-- server keeps, and how far its read log has been collected. New tables, so
-- an older version ignores them.
CREATE TABLE IF NOT EXISTS secrets_servers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    -- direct: argus-control calls url. tunnel: argus-secret dials
    -- argus-control and holds the connection, so it needs no inbound port.
    mode            TEXT NOT NULL DEFAULT 'direct' CHECK (mode IN ('direct', 'tunnel')),
    -- https://host[:port], as argus-control reaches it: directly, or through
    -- Cloudflare Tunnel or another proxy. Empty for a tunnel.
    url             TEXT NOT NULL CHECK (length(url) <= 300 AND
                        ((mode = 'direct' AND url LIKE 'https://%') OR (mode = 'tunnel' AND url = ''))),
    -- A tunnel server's own credential (sha256 hex), once it enrolled, and the
    -- one-time token it enrols with while it has not.
    credential_hash   TEXT UNIQUE CHECK (length(credential_hash) = 64),
    enroll_token_hash TEXT UNIQUE CHECK (length(enroll_token_hash) = 64),
    enroll_expires_at TIMESTAMPTZ,
    -- Its tunnel, as agent tunnels are kept (018_agent_tunnel.sql): the
    -- replica holding it, since when, and that replica's last ping. Any
    -- replica can be asked, so this is here and not in the holder's memory;
    -- a seen time older than tunnelFreshFor is a replica that went away.
    tunnel_replica      TEXT CHECK (length(tunnel_replica) <= 255),
    tunnel_connected_at TIMESTAMPTZ,
    tunnel_seen_at      TIMESTAMPTZ,
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

-- App tokens: callers outside Argus (a CI job, a service with no agent) that
-- read secrets straight from argus-secret. Only the SHA-256 of the token is
-- kept; it is pushed to the servers in the rules bundle as it is here.
CREATE TABLE IF NOT EXISTS secret_apps (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    token_hash  TEXT NOT NULL UNIQUE CHECK (length(token_hash) = 64),
    -- Its last four characters, to tell tokens apart.
    token_tail  TEXT NOT NULL DEFAULT '',
    expires_at  TIMESTAMPTZ NOT NULL,
    revoked_at  TIMESTAMPTZ,
    revoked_by  TEXT NOT NULL DEFAULT '',
    created_by  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Approvals for reading a secret whose rule needs one (secrets_requests.go).
-- Approved, it lets the person who asked read that one secret through the
-- console until expires_at; argus-secret is told the read was approved and
-- checks the rule itself.
CREATE TABLE IF NOT EXISTS secret_read_requests (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    requester_email TEXT NOT NULL,
    secret_name     TEXT NOT NULL CHECK (length(secret_name) BETWEEN 1 AND 255),
    reason          TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 500),
    minutes         INT  NOT NULL CHECK (minutes BETWEEN 5 AND 480),
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'approved', 'denied', 'cancelled')),
    decided_by      TEXT NOT NULL DEFAULT '',
    decided_at      TIMESTAMPTZ,
    decision_note   TEXT NOT NULL DEFAULT '' CHECK (length(decision_note) <= 500),
    expires_at      TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS secret_read_requests_requester_idx ON secret_read_requests (lower(requester_email), secret_name);
