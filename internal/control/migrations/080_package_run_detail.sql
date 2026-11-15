-- How a package run ended, in parts the console shows on their own:
-- whether the server changed, why it stopped, what to do next, and when it
-- reached each step. From the agent; NULL for runs before this and from
-- older agents, which the console reads from message as before.
--
-- Only an added nullable column: nothing to rewrite on a populated
-- database, and an older control plane ignores it.
ALTER TABLE package_runs ADD COLUMN IF NOT EXISTS detail JSONB;
