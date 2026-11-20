-- Website checks (websites.go): is a site up, and how much of the time.
-- New tables only, so an upgrade touches no existing row and an older
-- version ignores them.
CREATE TABLE IF NOT EXISTS website_checks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name            TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    url             TEXT NOT NULL CHECK (length(url) BETWEEN 1 AND 2000),
    -- The status the page must answer with; 0 takes any below 400.
    expect_status   INT  NOT NULL DEFAULT 0 CHECK (expect_status = 0 OR expect_status BETWEEN 100 AND 599),
    keyword         TEXT NOT NULL DEFAULT '' CHECK (length(keyword) <= 200),
    every_seconds   INT  NOT NULL DEFAULT 60 CHECK (every_seconds IN (60, 300, 600)),
    timeout_seconds INT  NOT NULL DEFAULT 30 CHECK (timeout_seconds BETWEEN 5 AND 60),
    -- The servers whose agents are asked for a second opinion, by asset id.
    confirm_from    UUID[] NOT NULL DEFAULT '{}' CHECK (cardinality(confirm_from) <= 3),
    paused          BOOLEAN NOT NULL DEFAULT false,
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- Where it stands. pending: not checked yet. unknown: Argus could not
    -- check (its own network was down), which is never "down".
    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'up', 'down', 'unknown')),
    state_since     TIMESTAMPTZ,
    -- Failed rounds in a row, confirmed from everywhere asked; down at 2.
    failing_rounds  INT NOT NULL DEFAULT 0,
    last_at         TIMESTAMPTZ,
    last_ms         INT,
    last_status     INT,
    last_detail     TEXT NOT NULL DEFAULT '',
    -- The last round's outcome, as website_rounds has it.
    last_outcome    TEXT NOT NULL DEFAULT '',
    cert_not_after  TIMESTAMPTZ,

    -- Scheduling across replicas: a replica takes a check by setting the
    -- lease, and sets next_at when the round is done.
    next_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS website_checks_due ON website_checks (next_at) WHERE NOT paused;

-- One row per round. outcome: up, partial (Argus could not reach it, a
-- chosen server could), pending (failed, not yet confirmed), down
-- (confirmed), unknown (Argus's own network was down; not counted).
-- Uptime is seconds up over seconds up or down.
CREATE TABLE IF NOT EXISTS website_rounds (
    check_id UUID        NOT NULL REFERENCES website_checks (id) ON DELETE CASCADE,
    at       TIMESTAMPTZ NOT NULL,
    secs     INT         NOT NULL,
    outcome  TEXT        NOT NULL CHECK (outcome IN ('up', 'partial', 'pending', 'down', 'unknown')),
    ms       INT,
    status   INT,
    detail   TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (check_id, at)
);
CREATE INDEX IF NOT EXISTS website_rounds_at ON website_rounds (at);

-- A confirmed outage: from the first failed round to the first good one.
CREATE TABLE IF NOT EXISTS website_outages (
    id         BIGSERIAL   PRIMARY KEY,
    check_id   UUID        NOT NULL REFERENCES website_checks (id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at   TIMESTAMPTZ,
    cause      TEXT        NOT NULL DEFAULT '',
    -- Who agreed it was down: "Argus" and the servers that looked.
    confirmed  TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS website_outages_check ON website_outages (check_id, started_at DESC);
-- At most one open outage per check.
CREATE UNIQUE INDEX IF NOT EXISTS website_outages_open ON website_outages (check_id) WHERE ended_at IS NULL;

-- Website news gets its own kind in the bell, so people can filter and mute it.
ALTER TABLE console_alerts DROP CONSTRAINT IF EXISTS console_alerts_kind_check;
ALTER TABLE console_alerts ADD CONSTRAINT console_alerts_kind_check
    CHECK (kind IN ('approval', 'signin', 'session', 'security', 'server', 'backup', 'data', 'updates', 'resources', 'cloud', 'website'));
