-- Alerts shown under the bell in the console header: an approval waiting, a
-- sign-in, a session opened, a security event. See console_alerts.go.
--
-- Not evidence. The audit chain holds what happened; this is a notice that
-- points at it, and is deleted after 30 days. audience decides who may read a
-- row (deciders, admins, or the one person named in recipient), so the query
-- that lists them is the access check and no handler filters afterwards.
CREATE TABLE IF NOT EXISTS console_alerts (
    seq       BIGSERIAL   PRIMARY KEY,
    at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    kind      TEXT        NOT NULL CHECK (kind IN ('approval', 'signin', 'session', 'security', 'server')),
    severity  TEXT        NOT NULL DEFAULT 'info' CHECK (severity IN ('info', 'warning', 'critical')),
    audience  TEXT        NOT NULL CHECK (audience IN ('deciders', 'admins', 'person')),
    -- The person, for audience 'person'; lower case.
    recipient TEXT        NOT NULL DEFAULT '',
    -- Who caused it, lower case. Nobody is alerted about their own act.
    actor     TEXT        NOT NULL DEFAULT '',
    title     TEXT        NOT NULL,
    detail    TEXT        NOT NULL DEFAULT '',
    -- A console path, never a URL: the console resolves it against itself.
    link      TEXT        NOT NULL DEFAULT '' CHECK (link = '' OR link LIKE '/%'),
    CHECK (audience <> 'person' OR recipient <> '')
);
CREATE INDEX IF NOT EXISTS console_alerts_at ON console_alerts (at);
CREATE INDEX IF NOT EXISTS console_alerts_recipient ON console_alerts (recipient, seq) WHERE audience = 'person';

-- Each person's choices, and how far they have read.
CREATE TABLE IF NOT EXISTS console_alert_prefs (
    email            TEXT        PRIMARY KEY,
    -- Every alert at or below read_seq is read; above it, those in
    -- console_alert_reads are.
    read_seq         BIGINT      NOT NULL DEFAULT 0,
    muted            TEXT[]      NOT NULL DEFAULT '{}',
    new_devices_only BOOLEAN     NOT NULL DEFAULT false,
    popup            BOOLEAN     NOT NULL DEFAULT true,
    sound            BOOLEAN     NOT NULL DEFAULT false,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Alerts read one at a time, above the person's read_seq. Deleted with the
-- alert, and folded into read_seq by "mark all read", so it stays small.
CREATE TABLE IF NOT EXISTS console_alert_reads (
    email TEXT   NOT NULL,
    seq   BIGINT NOT NULL REFERENCES console_alerts (seq) ON DELETE CASCADE,
    PRIMARY KEY (email, seq)
);
CREATE INDEX IF NOT EXISTS console_alert_reads_seq ON console_alert_reads (seq);
