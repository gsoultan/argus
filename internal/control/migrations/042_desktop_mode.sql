-- How the console opens a Windows server's desktop.
--
-- rdp:    the agent connects to the server's own Remote Desktop listener, as
--         the account, with the password the person types. The default, and
--         what every existing server keeps.
-- screen: the agent shares the console itself -- whatever is on the physical
--         screen, the sign-in screen included -- for a server with Remote
--         Desktop turned off, or an edition that has none.
--
-- A constant default, so adding it rewrites no rows.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS desktop_mode TEXT NOT NULL DEFAULT 'rdp'
    CHECK (desktop_mode IN ('rdp', 'screen'));
