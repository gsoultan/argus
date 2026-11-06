-- Android devices as assets.
--
-- A phone or tablet is reached through the Argus app on it, which holds the
-- same tunnel a server's agent holds. The console opens its screen the way
-- it opens a Windows server set to screen sharing, so the row stays an
-- asset: what differs is the kind, how a session on it is agreed to, and
-- whether control needs an approval first.
--
-- kind:                    'server' (everything that exists today) or
--                          'android'.
-- control_mode:            'attended' asks whoever holds the device before
--                          the screen is shared, every session; 'unattended'
--                          asks nobody, for company devices nobody holds.
-- control_needs_approval:  the 'control' principal needs an approved access
--                          request first, as root does on a server; 'view'
--                          never does.
--
-- Constant defaults, so adding them rewrites no rows, and an older control
-- plane reading assets ignores them.

ALTER TABLE assets ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT 'server'
    CHECK (kind IN ('server', 'android'));
ALTER TABLE assets ADD COLUMN IF NOT EXISTS control_mode TEXT NOT NULL DEFAULT 'attended'
    CHECK (control_mode IN ('attended', 'unattended'));
ALTER TABLE assets ADD COLUMN IF NOT EXISTS control_needs_approval BOOLEAN NOT NULL DEFAULT false;
