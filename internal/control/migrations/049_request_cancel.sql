-- A request the person who asked takes back, and alerts that know when what
-- they asked for is over. See requests.go (CancelRequest) and
-- console_alerts.go (closeAlerts).
--
-- access_requests.state has no CHECK; 'cancelled' joins its values here in
-- words only.

-- What an alert is about ("request:<id>"), and how that ended. A closed
-- alert stops counting as unread and asks nobody to act.
ALTER TABLE console_alerts ADD COLUMN IF NOT EXISTS subject   TEXT NOT NULL DEFAULT '';
ALTER TABLE console_alerts ADD COLUMN IF NOT EXISTS closed    TEXT NOT NULL DEFAULT '';
ALTER TABLE console_alerts ADD COLUMN IF NOT EXISTS closed_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS console_alerts_subject   ON console_alerts (subject)   WHERE subject <> '';
CREATE INDEX IF NOT EXISTS console_alerts_closed_at ON console_alerts (closed_at) WHERE closed_at IS NOT NULL;

-- Alerts raised before this knew their request only by link. Change requests'
-- links name it; tie those to it, and close the ones already settled, so an
-- upgrade does not leave decided changes asking to be decided.
UPDATE console_alerts SET subject = 'request:' || substring(link FROM length('/approvals?req=') + 1)
 WHERE subject = '' AND kind = 'approval' AND link LIKE '/approvals?req=%'
   -- Only what asked someone to decide; the asker's own "was approved" is news.
   AND title NOT LIKE 'Your request %';
UPDATE console_alerts a SET
       closed = CASE r.state WHEN 'approved' THEN 'Approved by ' || coalesce(r.decided_by_email, 'someone')
                             WHEN 'denied'   THEN 'Denied by '   || coalesce(r.decided_by_email, 'someone')
                             ELSE 'No longer waiting' END,
       closed_at = coalesce(r.decided_at, now())
  FROM access_requests r
 WHERE a.subject = 'request:' || r.id::text AND a.closed_at IS NULL AND r.state <> 'pending';

-- A restore whose request was cancelled.
ALTER TABLE database_restores DROP CONSTRAINT IF EXISTS database_restores_state_check;
ALTER TABLE database_restores ADD CONSTRAINT database_restores_state_check
    CHECK (state IN ('pending', 'denied', 'cancelled', 'running', 'done', 'failed', 'undone'));
