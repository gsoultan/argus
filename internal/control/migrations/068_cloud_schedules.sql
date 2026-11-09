-- Scheduled start and stop for cloud instances.
--
-- A schedule is a named set of weekly windows in one time zone. An
-- instance joins one with the tag ArgusSchedule=<name>; the control plane
-- starts it when a window opens and stops it when the window closes. Every
-- transition is a row in cloud_schedule_runs, which is also how two control
-- hosts agree on who runs it: the first to insert the row owns it.
--
-- Only new tables: nothing to rewrite on a populated database.

CREATE TABLE IF NOT EXISTS cloud_schedules (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL CHECK (name ~ '^[A-Za-z0-9-]{1,63}$'),
    timezone       TEXT NOT NULL,
    -- [{days: [1..7] (Monday is 1), start: "08:00", stop: "19:00",
    --   stopDay: 1..7 (when the stop falls on another day)}]
    windows        JSONB NOT NULL DEFAULT '[]',
    -- Days (YYYY-MM-DD), in the schedule's zone, on which no window opens.
    skip_dates     TEXT[] NOT NULL DEFAULT '{}',
    -- Empty means every account and every region.
    account_ids    UUID[] NOT NULL DEFAULT '{}',
    regions        TEXT[] NOT NULL DEFAULT '{}',
    mode           TEXT NOT NULL DEFAULT 'transitions' CHECK (mode IN ('transitions', 'enforce')),
    -- Who runs it: the person whose version is in effect. Calls to AWS are
    -- made as them, so CloudTrail names a person.
    owner          TEXT NOT NULL,
    state          TEXT NOT NULL CHECK (state IN ('awaiting_approval', 'active', 'paused')),
    paused_reason  TEXT NOT NULL DEFAULT '',
    -- An edit waiting for a decision, while the version above stays in
    -- effect. A new schedule waits in state awaiting_approval instead.
    pending        JSONB,
    pending_by     TEXT,
    pending_at     TIMESTAMPTZ,
    reason         TEXT NOT NULL DEFAULT '',
    -- Up to when the loop has looked for transitions.
    checked_at     TIMESTAMPTZ,
    created_by     TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by     TEXT NOT NULL,
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS cloud_schedules_name ON cloud_schedules (lower(name));

CREATE TABLE IF NOT EXISTS cloud_schedule_runs (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    schedule_id  UUID NOT NULL REFERENCES cloud_schedules (id) ON DELETE CASCADE,
    due_at       TIMESTAMPTZ NOT NULL,
    action       TEXT NOT NULL CHECK (action IN ('start', 'stop')),
    state        TEXT NOT NULL CHECK (state IN ('running', 'done', 'partial', 'failed', 'skipped')),
    -- One entry per instance: what was done to it, or why not.
    targets      JSONB NOT NULL DEFAULT '[]',
    request_ids  TEXT[] NOT NULL DEFAULT '{}',
    detail       TEXT NOT NULL DEFAULT '',
    attempts     INTEGER NOT NULL DEFAULT 0,
    -- When the loop looks at this run again: a retry, or an instance
    -- waiting for a session to end. NULL once it is settled.
    next_at      TIMESTAMPTZ,
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS cloud_schedule_runs_once ON cloud_schedule_runs (schedule_id, due_at, action);
CREATE INDEX IF NOT EXISTS cloud_schedule_runs_next ON cloud_schedule_runs (next_at) WHERE next_at IS NOT NULL;

-- Keep running: one instance is left alone by every schedule until a time.
CREATE TABLE IF NOT EXISTS cloud_keep_running (
    account_id   UUID NOT NULL REFERENCES cloud_accounts (id),
    instance_id  TEXT NOT NULL,
    until        TIMESTAMPTZ NOT NULL,
    reason       TEXT NOT NULL DEFAULT '',
    set_by       TEXT NOT NULL,
    set_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (account_id, instance_id)
);
