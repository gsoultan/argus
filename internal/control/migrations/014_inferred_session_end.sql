-- Closing a session Argus watched disappear, and saying that is what happened.
--
-- A gateway killed rather than drained never reports the end, so the row stays
-- `active` forever: fifteen in dev, the oldest for nineteen days. Migration 013
-- made that silence measurable and the console stopped calling them live, but
-- nothing ever closes them, so the unknown count only grows.
--
-- The end time written is `last_reported_at`, never `now()`. That is the last
-- moment the session was observed alive -- a lower bound Argus actually saw.
-- The session ended at some unknown point at or after it, and recording the
-- observation rather than the guess is the difference between a log that says
-- "alive at least until here" and one that invents a minute.
--
-- `end_inferred` is what keeps the two apart. Without it a closed session with
-- an end time reads as a clean logout, and an auditor would have no way to know
-- the control plane deduced it. Every surface that shows an end must be able to
-- say which kind it is.
ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS end_inferred BOOLEAN NOT NULL DEFAULT false;
