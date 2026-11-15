-- Risks & waste: a finding a person chose to keep. It is hidden from the
-- list until it expires, or, with until_change, until what it rests on
-- (basis) changes. Who and why are shown to everyone with Cloud access and
-- written to the audit chain when kept and when taken back.
CREATE TABLE IF NOT EXISTS cloud_finding_keeps (
    account_id  UUID NOT NULL REFERENCES cloud_accounts (id),
    finding_id  TEXT NOT NULL CHECK (length(finding_id) BETWEEN 1 AND 300),
    basis       TEXT NOT NULL,
    reason      TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 300),
    kept_by     TEXT NOT NULL,
    kept_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ,
    until_change BOOLEAN NOT NULL DEFAULT false,
    PRIMARY KEY (account_id, finding_id)
);
