-- Logins while Argus cannot be reached (outage_logins.go).
--
-- A server set to refuse direct SSH, or to ask for approval, refuses every
-- plain SSH login while its agent cannot reach Argus. This lets members of
-- named groups in once the agent has not heard from Argus for a while, still
-- recorded. Off (0) unless someone turns it on, including for the row an
-- upgrade already holds, so upgrading changes nothing. Only new columns.
ALTER TABLE gateway_policy ADD COLUMN IF NOT EXISTS outage_logins_after INT NOT NULL DEFAULT 0;
ALTER TABLE gateway_policy DROP CONSTRAINT IF EXISTS gateway_policy_outage_logins_after_check;
ALTER TABLE gateway_policy ADD CONSTRAINT gateway_policy_outage_logins_after_check
    CHECK (outage_logins_after IN (0, 5, 15, 60));
ALTER TABLE gateway_policy ADD COLUMN IF NOT EXISTS outage_logins_groups TEXT[] NOT NULL
    DEFAULT ARRAY['sudo', 'wheel', 'admin'];

-- A server whose page turned it off: it never lets anyone in this way.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS outage_logins_off BOOLEAN NOT NULL DEFAULT false;
