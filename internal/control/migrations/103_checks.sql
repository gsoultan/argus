-- argus:breaking: website checks move to the checks table; going back shows them, their uptime and outages as they were before the update
-- Monitoring's shared check engine (checks.go, websites.go). One table of
-- checks of every kind, with what they share as columns and what a kind adds
-- as settings; one table of rounds and one of outages. Website checks are
-- the first kind and are copied in here with their ids, rounds and outages,
-- so nothing anyone sees changes: uptime, outages and the bell's
-- "website:<id>" alerts carry on.
--
-- website_checks, website_rounds and website_outages are left as they are
-- and no longer written, so the previous version still starts and finds the
-- checks it had (as migration 100 did with website_alert_settings).

CREATE TABLE IF NOT EXISTS checks (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Which kind: what it checks and how (checkKinds in checks.go).
    kind            TEXT NOT NULL CHECK (kind IN ('website')),
    name            TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    -- What the kind needs to check it, as the kind defines it. A website's:
    -- url, expectStatus, keyword, method, body, headerNames.
    settings        JSONB NOT NULL DEFAULT '{}'::jsonb
                    CHECK (jsonb_typeof(settings) = 'object' AND octet_length(settings::text) <= 80000),
    -- The kind's secrets, sealed with a key derived from signing_secret and
    -- never returned (a website's header values).
    secrets_sealed  BYTEA,
    every_seconds   INT  NOT NULL DEFAULT 60 CHECK (every_seconds IN (60, 300, 600)),
    timeout_seconds INT  NOT NULL DEFAULT 30 CHECK (timeout_seconds BETWEEN 5 AND 60),
    -- The servers whose agents also check it, by asset id.
    confirm_from    UUID[] NOT NULL DEFAULT '{}' CHECK (cardinality(confirm_from) <= 3),
    paused          BOOLEAN NOT NULL DEFAULT false,
    telegram        TEXT NOT NULL DEFAULT 'default' CHECK (telegram IN ('default', 'off', 'chats')),
    telegram_bot    TEXT NOT NULL DEFAULT '' CHECK (length(telegram_bot) <= 40),
    telegram_chats  TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(telegram_chats) <= 20),
    created_by      TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),

    state           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'up', 'down', 'unknown')),
    state_since     TIMESTAMPTZ,
    failing_rounds  INT NOT NULL DEFAULT 0,
    last_at         TIMESTAMPTZ,
    last_ms         INT,
    -- The kind's answer code, when it has one (a website's HTTP status).
    last_status     INT,
    last_detail     TEXT NOT NULL DEFAULT '',
    last_outcome    TEXT NOT NULL DEFAULT '',
    -- When what the check watches runs out (a website's certificate).
    expires_at      TIMESTAMPTZ,

    next_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    lease_until     TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS checks_due ON checks (next_at) WHERE NOT paused;
CREATE INDEX IF NOT EXISTS checks_kind ON checks (kind);

CREATE TABLE IF NOT EXISTS check_rounds (
    check_id UUID        NOT NULL REFERENCES checks (id) ON DELETE CASCADE,
    at       TIMESTAMPTZ NOT NULL,
    secs     INT         NOT NULL,
    outcome  TEXT        NOT NULL CHECK (outcome IN ('up', 'partial', 'pending', 'down', 'unknown')),
    ms       INT,
    status   INT,
    detail   TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (check_id, at)
);
CREATE INDEX IF NOT EXISTS check_rounds_at ON check_rounds (at);

CREATE TABLE IF NOT EXISTS check_outages (
    id         BIGSERIAL   PRIMARY KEY,
    check_id   UUID        NOT NULL REFERENCES checks (id) ON DELETE CASCADE,
    started_at TIMESTAMPTZ NOT NULL,
    ended_at   TIMESTAMPTZ,
    cause      TEXT        NOT NULL DEFAULT '',
    confirmed  TEXT        NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS check_outages_check ON check_outages (check_id, started_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS check_outages_open ON check_outages (check_id) WHERE ended_at IS NULL;

-- The website checks, as they are, with their ids. A check's lease is not
-- carried over: a round a previous version was running records nothing here.
INSERT INTO checks (id, kind, name, settings, secrets_sealed, every_seconds, timeout_seconds, confirm_from,
                    paused, telegram, telegram_bot, telegram_chats, created_by, created_at, updated_at,
                    state, state_since, failing_rounds, last_at, last_ms, last_status, last_detail,
                    last_outcome, expires_at, next_at)
SELECT id, 'website', name,
       jsonb_build_object('url', url, 'expectStatus', expect_status, 'keyword', keyword,
                          'method', method, 'body', request_body, 'headerNames', to_jsonb(header_names)),
       headers_sealed, every_seconds, timeout_seconds, confirm_from,
       paused, telegram, telegram_bot, telegram_chats, created_by, created_at, updated_at,
       state, state_since, failing_rounds, last_at, last_ms, last_status, last_detail,
       last_outcome, cert_not_after, next_at
FROM website_checks
ON CONFLICT (id) DO NOTHING;

INSERT INTO check_rounds (check_id, at, secs, outcome, ms, status, detail)
SELECT check_id, at, secs, outcome, ms, status, detail FROM website_rounds
ON CONFLICT DO NOTHING;

INSERT INTO check_outages (id, check_id, started_at, ended_at, cause, confirmed)
SELECT id, check_id, started_at, ended_at, cause, confirmed FROM website_outages
ON CONFLICT DO NOTHING;
SELECT setval(pg_get_serial_sequence('check_outages', 'id'),
              COALESCE((SELECT max(id) FROM check_outages), 1),
              (SELECT max(id) FROM check_outages) IS NOT NULL);
