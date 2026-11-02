-- Every agent heartbeat finds its asset by full hostname, by agent_hostname,
-- or by the short name (split_part(hostname, '.', 1)): agents rarely know the
-- fully qualified name the inventory uses. The first two had indexes and the
-- third did not, so the OR could not use any of them and every heartbeat read
-- the whole inventory -- 33 sequential scans a second for a thousand agents.
--
-- With this index the planner ORs three index probes. The table is the
-- inventory, small enough that building it under the migration's lock takes
-- moments on a populated database.
CREATE INDEX IF NOT EXISTS assets_short_hostname_idx
    ON assets (split_part(hostname, '.', 1));
