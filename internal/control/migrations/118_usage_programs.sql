-- Which programs used the server in each 5-minute bucket (usage_history.go):
-- the Metrics tab's "Busiest programs" for any point of the last 30 days.
-- A JSON object per bucket, keyed by program name, of the busiest moment's
-- processor share, memory, disk bytes per second read and written, and how
-- many processes ran under the name; only the programs the agent named in a
-- report (at most 15 each) are kept. A nullable column, so an older version
-- ignores it, and the hourly roll-up does not carry it.
ALTER TABLE usage_history ADD COLUMN IF NOT EXISTS programs JSONB;
