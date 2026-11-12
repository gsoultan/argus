-- An asset's alias: the name people know it by ("Payroll database",
-- "Front desk PC"), shown in the console in place of the hostname. The
-- hostname stays what it was, the DNS name Argus, its agent and a
-- certificate go by, so the alias is free text and may be left empty.
--
-- A label only. Nothing that decides whether a session opens reads it,
-- and sessions and the audit chain keep naming the hostname.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS alias TEXT NOT NULL DEFAULT '';
