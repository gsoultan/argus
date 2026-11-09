-- Cloud, phase 6: costs and budgets. An account's last billing read is kept
-- whole, with when it was read and why the last read failed, if it did.
CREATE TABLE IF NOT EXISTS cloud_costs (
    account_id UUID PRIMARY KEY REFERENCES cloud_accounts (id),
    read_at    TIMESTAMPTZ,
    costs      JSONB,
    error      TEXT NOT NULL DEFAULT '',
    error_at   TIMESTAMPTZ,
    -- When a person last asked for a read, so asking again soon is refused.
    asked_at   TIMESTAMPTZ
);

-- A monthly budget for one account, or for every account when account_id
-- is NULL. Each threshold, and the forecast passing the amount, alerts
-- once a month; alerted_month and alerted say which have this month.
CREATE TABLE IF NOT EXISTS cloud_budgets (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    UUID REFERENCES cloud_accounts (id),
    name          TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    amount        NUMERIC NOT NULL CHECK (amount > 0),
    thresholds    INTEGER[] NOT NULL DEFAULT '{80,100}',
    forecast      BOOLEAN NOT NULL DEFAULT true,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    alerted_month TEXT NOT NULL DEFAULT '',
    alerted       INTEGER[] NOT NULL DEFAULT '{}'
);
CREATE INDEX IF NOT EXISTS cloud_budgets_account ON cloud_budgets (account_id);
