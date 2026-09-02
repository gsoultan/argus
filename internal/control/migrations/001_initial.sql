-- Argus control plane schema.
--
-- Shapes mirror web/src/types/domain.ts, which is the contract the console was
-- built against. Where the two must agree, the TypeScript is the source of
-- truth and this follows it.

CREATE TABLE IF NOT EXISTS assets (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    hostname                TEXT NOT NULL UNIQUE,
    address                 TEXT NOT NULL,
    port                    INT  NOT NULL DEFAULT 22,
    os                      TEXT NOT NULL DEFAULT '',
    tags                    TEXT[] NOT NULL DEFAULT '{}',
    group_name              TEXT NOT NULL DEFAULT 'ungrouped',

    -- 'injected-key' | 'injected-password' | 'ca-certificate'
    credential_mode         TEXT NOT NULL DEFAULT 'injected-key',
    credential_rotated_at   TIMESTAMPTZ,
    rotation_interval_days  INT,

    -- The trust anchor. Argus terminates SSH, so an unpinned target is one
    -- nobody has verified.
    host_key_state          TEXT NOT NULL DEFAULT 'unpinned',
    host_key_fingerprint    TEXT,
    host_key_pinned_at      TIMESTAMPTZ,

    health                  TEXT NOT NULL DEFAULT 'reachable',
    last_checked_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    principals              TEXT[] NOT NULL DEFAULT '{}',

    -- Agent coverage. Without an agent a direct connection to port 22 leaves
    -- no trace at all, so absence is a coverage gap, not a cosmetic detail.
    agent_state             TEXT NOT NULL DEFAULT 'absent',
    agent_last_seen_at      TIMESTAMPTZ,
    bypass_posture          TEXT NOT NULL DEFAULT 'open',
    unmanaged_key_count     INT  NOT NULL DEFAULT 0,

    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email         TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'operator',
    idp_subject   TEXT,
    mfa_enrolled  BOOLEAN NOT NULL DEFAULT false,
    last_seen_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS sessions (
    id             UUID PRIMARY KEY,
    user_email     TEXT NOT NULL,
    asset_id       UUID REFERENCES assets(id) ON DELETE SET NULL,
    asset_hostname TEXT NOT NULL,
    principal      TEXT NOT NULL,
    protocol       TEXT NOT NULL DEFAULT 'ssh',

    -- 'brokered' | 'direct'. A direct session was recorded but no policy ran,
    -- which makes it a control failure even when the user was authorised.
    origin         TEXT NOT NULL DEFAULT 'brokered',
    origin_reason  TEXT,

    state          TEXT NOT NULL DEFAULT 'active',
    started_at     TIMESTAMPTZ NOT NULL,
    ended_at       TIMESTAMPTZ,
    client_ip      TEXT NOT NULL DEFAULT '',

    -- 'pty' | 'ebpf' | 'none'
    fidelity       TEXT NOT NULL DEFAULT 'pty',
    recording_bytes BIGINT NOT NULL DEFAULT 0,
    recording_path TEXT,
    command_count  INT,
    exit_code      INT,

    -- Head of the recording's hash chain, so the artefact can be verified
    -- against what the gateway sealed.
    chain_head     TEXT,
    risk_flags     TEXT[] NOT NULL DEFAULT '{}',

    -- Which host reported it, for reconciling a session recorded at both ends.
    reported_by    TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS sessions_started_idx ON sessions (started_at DESC);
CREATE INDEX IF NOT EXISTS sessions_state_idx   ON sessions (state);
-- Bypasses are queried far more often than their share of rows, and only ever
-- as a filter, so a partial index keeps it small.
CREATE INDEX IF NOT EXISTS sessions_direct_idx  ON sessions (started_at DESC)
    WHERE origin = 'direct';

CREATE TABLE IF NOT EXISTS access_requests (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    requester_email  TEXT NOT NULL,
    asset_hostnames  TEXT[] NOT NULL DEFAULT '{}',
    principal        TEXT NOT NULL,
    justification    TEXT NOT NULL,
    duration_minutes INT  NOT NULL,
    state            TEXT NOT NULL DEFAULT 'pending',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_at       TIMESTAMPTZ,
    decided_by_email TEXT,
    decision_note    TEXT,
    expires_at       TIMESTAMPTZ,
    break_glass      BOOLEAN NOT NULL DEFAULT false
);

-- Append-only, hash-chained. `seq` is assigned by the database so concurrent
-- writers cannot produce a gap or a duplicate, and the chain is computed in
-- order of that sequence.
CREATE TABLE IF NOT EXISTS audit_events (
    seq          BIGSERIAL PRIMARY KEY,
    id           UUID NOT NULL DEFAULT gen_random_uuid(),
    at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    action       TEXT NOT NULL,
    severity     TEXT NOT NULL DEFAULT 'info',
    actor_email  TEXT NOT NULL DEFAULT '',
    target       TEXT NOT NULL DEFAULT '',
    detail       TEXT NOT NULL DEFAULT '',
    prev_hash    TEXT NOT NULL,
    hash         TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS audit_at_idx       ON audit_events (at DESC);
CREATE INDEX IF NOT EXISTS audit_severity_idx ON audit_events (severity);

-- Agent liveness. Silence is the interesting state: a user with root can kill
-- the agent, but they cannot make the absence look like health.
CREATE TABLE IF NOT EXISTS agents (
    hostname        TEXT PRIMARY KEY,
    version         TEXT NOT NULL DEFAULT '',
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    active_sessions INT NOT NULL DEFAULT 0,
    posture         JSONB,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
