-- The whole firewall on each server, as its agent last read it
-- (tunnel/hostfirewall.go): which firewall is in charge, whether it filters,
-- its defaults and every rule that is switched on. Read only; the Firewall
-- tab and Assets › Firewalls show it.
--
-- NULL is "not reported", from an agent older than firewall reading. The
-- previous version starts on this database and ignores the column.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS firewall JSONB
    CHECK (firewall IS NULL OR octet_length(firewall::text) <= 400000);
