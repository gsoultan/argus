-- More of what the agent measures, kept with the rest of its history
-- (usage_history.go): the 1-minute load average, processor steal, TCP
-- connections open, and disk activity summed over the physical disks. Sums
-- and counts as for processor and network, so a bucket's average stays
-- exact however many reports it folds; maxima are the busiest report.
ALTER TABLE usage_history
    ADD COLUMN IF NOT EXISTS load_max   REAL,
    ADD COLUMN IF NOT EXISTS steal_sum  DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS steal_n    INTEGER          NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS steal_max  REAL,
    ADD COLUMN IF NOT EXISTS tcp_max    INTEGER,
    ADD COLUMN IF NOT EXISTS rops_sum   DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS wops_sum   DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS rbps_sum   DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS wbps_sum   DOUBLE PRECISION NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS io_n       INTEGER          NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS iops_max   DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS io_bps_max DOUBLE PRECISION;

-- The same history rolled up by the hour and kept 13 months, so a
-- reservation or a right-sizing is judged on a year rather than a month.
-- Built from usage_history by the hourly prune; one row per agent per hour,
-- about 9,500 per agent. The same columns, so one reader serves both.
CREATE TABLE IF NOT EXISTS usage_hourly (
    hostname   TEXT        NOT NULL,
    bucket     TIMESTAMPTZ NOT NULL,
    samples    INTEGER     NOT NULL DEFAULT 0,
    cpu_sum    DOUBLE PRECISION NOT NULL DEFAULT 0,
    cpu_n      INTEGER     NOT NULL DEFAULT 0,
    cpu_max    REAL,
    mem_pct    REAL,
    swap_pct   REAL,
    disks      JSONB,
    rx_sum     DOUBLE PRECISION NOT NULL DEFAULT 0,
    tx_sum     DOUBLE PRECISION NOT NULL DEFAULT 0,
    net_n      INTEGER     NOT NULL DEFAULT 0,
    rx_max     DOUBLE PRECISION,
    tx_max     DOUBLE PRECISION,
    load_max   REAL,
    steal_sum  DOUBLE PRECISION NOT NULL DEFAULT 0,
    steal_n    INTEGER     NOT NULL DEFAULT 0,
    steal_max  REAL,
    tcp_max    INTEGER,
    rops_sum   DOUBLE PRECISION NOT NULL DEFAULT 0,
    wops_sum   DOUBLE PRECISION NOT NULL DEFAULT 0,
    rbps_sum   DOUBLE PRECISION NOT NULL DEFAULT 0,
    wbps_sum   DOUBLE PRECISION NOT NULL DEFAULT 0,
    io_n       INTEGER     NOT NULL DEFAULT 0,
    iops_max   DOUBLE PRECISION,
    io_bps_max DOUBLE PRECISION,
    PRIMARY KEY (hostname, bucket)
);
CREATE INDEX IF NOT EXISTS usage_hourly_bucket ON usage_hourly (bucket);
