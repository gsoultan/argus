-- Who ended a session, and why.
--
-- The gateway has always sent these on a terminated session. The control plane
-- had nowhere to put them, and decode() rejects unknown fields on purpose -- so
-- the entire report came back 400 and was spooled, retried, and rejected again.
--
-- The effect was that terminating a session, which is one of the controls this
-- product exists to offer, never reached the record. The row stayed `active`
-- with no chain head and no recording key, for a session that had in fact ended
-- minutes earlier and whose recording was sealed and uploaded. The console
-- showed a live session that was not, and an auditor asking why it ended had
-- nothing to read.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS terminated_by      TEXT;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS termination_reason TEXT;

-- Terminated sessions are what an auditor filters for first.
CREATE INDEX IF NOT EXISTS sessions_terminated_idx
    ON sessions (terminated_by) WHERE terminated_by IS NOT NULL;
