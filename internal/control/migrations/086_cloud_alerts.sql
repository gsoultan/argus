-- Cloud news gets its own kind in the bell, so people can filter and mute
-- it: reservations ending in 30 and 7 days. See cloud_reservation_alerts.go.
ALTER TABLE console_alerts DROP CONSTRAINT IF EXISTS console_alerts_kind_check;
ALTER TABLE console_alerts ADD CONSTRAINT console_alerts_kind_check
    CHECK (kind IN ('approval', 'signin', 'session', 'security', 'server', 'backup', 'data', 'updates', 'resources', 'cloud'));
