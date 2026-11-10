-- Assets running low on disk or memory get their own kind in the bell, so
-- people can filter and mute them: one alert per asset, resource and level
-- per day. See resources.go.
ALTER TABLE console_alerts DROP CONSTRAINT IF EXISTS console_alerts_kind_check;
ALTER TABLE console_alerts ADD CONSTRAINT console_alerts_kind_check
    CHECK (kind IN ('approval', 'signin', 'session', 'security', 'server', 'backup', 'data', 'updates', 'resources'));
