-- Resource history per agent, in 5-minute buckets, kept 30 days: the
-- Metrics tab's charts. Each usage report (about one a minute) is folded
-- into its bucket with the heartbeat that carries it. See usage_history.go.
CREATE TABLE IF NOT EXISTS usage_history (
    hostname  TEXT        NOT NULL,
    bucket    TIMESTAMPTZ NOT NULL,
    samples   INTEGER     NOT NULL DEFAULT 0,
    cpu_sum   DOUBLE PRECISION NOT NULL DEFAULT 0,
    cpu_n     INTEGER     NOT NULL DEFAULT 0,
    cpu_max   REAL,
    mem_pct   REAL,
    swap_pct  REAL,
    disks     JSONB,
    rx_sum    DOUBLE PRECISION NOT NULL DEFAULT 0,
    tx_sum    DOUBLE PRECISION NOT NULL DEFAULT 0,
    net_n     INTEGER     NOT NULL DEFAULT 0,
    rx_max    DOUBLE PRECISION,
    tx_max    DOUBLE PRECISION,
    PRIMARY KEY (hostname, bucket)
);
CREATE INDEX IF NOT EXISTS usage_history_bucket ON usage_history (bucket);
