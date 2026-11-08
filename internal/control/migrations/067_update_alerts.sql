-- Update notices get their own kind in the bell, so people can filter and
-- mute them: one digest per kind per day of what is waiting across the
-- fleet (security fixes, agents behind, restarts). See updates_waiting.go.
ALTER TABLE console_alerts DROP CONSTRAINT IF EXISTS console_alerts_kind_check;
ALTER TABLE console_alerts ADD CONSTRAINT console_alerts_kind_check
    CHECK (kind IN ('approval', 'signin', 'session', 'security', 'server', 'backup', 'data', 'updates'));
