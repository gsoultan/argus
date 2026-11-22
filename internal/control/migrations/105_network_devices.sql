-- Monitoring: network devices read over SNMP (network_devices.go), the
-- second kind of check. A device's settings are in checks.settings (host,
-- port, version, user, protocols, the interfaces to watch) and its
-- credentials sealed in secrets_sealed; the servers it is read from are
-- confirm_from, of which it needs at least one.
--
-- last_data is what a kind keeps from its last answer: for a device, its
-- name, uptime and interfaces with their counters, so the page can show
-- them and the next round can tell a restart and traffic rates.
--
-- The previous version starts on this database: it reads only website
-- checks, and ignores the new kind and column.

ALTER TABLE checks DROP CONSTRAINT IF EXISTS checks_kind_check;
ALTER TABLE checks ADD CONSTRAINT checks_kind_check CHECK (kind IN ('website', 'snmp'));

ALTER TABLE checks ADD COLUMN IF NOT EXISTS last_data JSONB NOT NULL DEFAULT '{}'::jsonb
    CHECK (jsonb_typeof(last_data) = 'object' AND octet_length(last_data::text) <= 400000);
