-- One credential per host, instead of one for the whole fleet.
--
-- Until now every agent reported with ARGUS_REPORTER_TOKEN: the same bearer
-- string every gateway holds. Root on any one managed host could read it out of
-- the agent's unit and report sessions, heartbeats and facts as any other host,
-- or as a gateway -- which made "this host said so" worth nothing as evidence.
--
-- Now an administrator mints a short-lived, single-use enrollment token in the
-- console; the agent exchanges it once for a credential that is bound to one
-- hostname, can only report as that host, and can be revoked on its own. The
-- shared token stays, for gateways only.

-- A token an administrator handed to whoever installs an agent.
--
-- Only the hash is stored. The token is shown once, when it is made, and a
-- database dump must not be a supply of ways to enroll a host.
CREATE TABLE IF NOT EXISTS agent_enrollment_tokens (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    token_hash   TEXT NOT NULL UNIQUE,
    -- Empty means any hostname may use it. Set, it is the only one that may:
    -- a token copied off a terminal then cannot enroll a different machine.
    hostname     TEXT NOT NULL DEFAULT '',
    created_by   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    -- Single use. Consumed in the same statement that checks it, so two agents
    -- racing with one token cannot both win.
    used_at      TIMESTAMPTZ,
    used_by_host TEXT
);

-- What an enrolled agent reports with.
--
-- Revoked, never deleted: sessions an agent reported are evidence, and which
-- credential vouched for them, and whether it was later withdrawn, is part of
-- reading that evidence.
CREATE TABLE IF NOT EXISTS agent_credentials (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    hostname    TEXT NOT NULL,
    secret_hash TEXT NOT NULL UNIQUE,
    token_id    UUID NOT NULL REFERENCES agent_enrollment_tokens(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at  TIMESTAMPTZ,
    revoked_by  TEXT
);

-- One live credential per host. A second enrollment for a host that already
-- has one is refused rather than replacing it: otherwise anyone holding an
-- unbound token could take over an existing host's identity, and the real
-- agent would find out only when its reports started failing. Revoke first.
--
-- On lower(hostname) because hostnames are not case-sensitive and the agent
-- reports whatever the kernel says.
CREATE UNIQUE INDEX IF NOT EXISTS agent_credentials_live_host_idx
    ON agent_credentials (lower(hostname)) WHERE revoked_at IS NULL;

-- "Which host did this token enroll" is answered from the token row; this is
-- for the reverse, and so deleting a token never has to scan.
CREATE INDEX IF NOT EXISTS ix_agent_credentials_token_id ON agent_credentials (token_id);
