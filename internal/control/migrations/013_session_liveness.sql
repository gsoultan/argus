-- When a session was last heard about, so silence can be told from life.
--
-- Fifteen sessions in dev have been `active` since early September, the oldest
-- for nineteen days. Nothing reaps them, because nothing can: a gateway that is
-- killed rather than drained never reports the end, gateways carry no identity
-- the control plane could attribute orphans to, and there is no maximum session
-- duration -- so age alone cannot tell a dead session from a long-running one.
--
-- This does not reap anything. A session we have lost track of has not closed
-- and is not running; it is unknown, and the honest rendering is to say when we
-- last heard from it. That is already the pattern for agents, where `stale`
-- means "stopped reporting" and the console says how long ago -- treat silence
-- as hostile until proven otherwise.
--
-- Defaulted to now() rather than started_at for the backfill. An existing row
-- has never been stamped, and dating it from its start would invent nineteen
-- days of silence we did not observe; the truth is that this column began
-- knowing nothing, and every subsequent report corrects it.
ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS last_reported_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- No index on the new column. The reader is "which active sessions have gone
-- quiet", and sessions_state_idx already narrows that to the live set, which is
-- bounded by concurrency rather than by history -- a few hundred rows at the
-- published ceiling, against the whole session log. A partial index on
-- last_reported_at was tried and the planner would not take it, while every
-- keepalive would have paid to maintain it: once a minute per live session.
