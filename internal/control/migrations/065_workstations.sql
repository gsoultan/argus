-- Workstations: a laptop or PC someone sits at, next to servers and devices.
--
-- A workstation is stored and opened exactly as a server is (protocol ssh or
-- rdp, its own accounts); the kind says a person uses it, which the console
-- shows as its type and which decides, in a later step, who is asked before
-- a session opens. Only a widened CHECK: nothing here can fail on rows that
-- exist, and an older control plane reading assets sees a kind it shows as
-- a server.

ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_kind_check;
ALTER TABLE assets ADD CONSTRAINT assets_kind_check
    CHECK (kind IN ('server', 'workstation', 'android'));
