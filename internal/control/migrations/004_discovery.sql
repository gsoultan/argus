-- Asset discovery.
--
-- Every asset until now was hand-written into the inventory, which makes
-- "Argus covers all privileged access" a claim rather than something anyone can
-- check. The host nobody remembered to add is exactly the one that gets
-- compromised. These columns let an agent say what it is, so the control plane
-- can compare the hosts that exist against the hosts the inventory knows about.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS fqdn       TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS machine_id TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS os         TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS addresses  TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS accounts   TEXT[] NOT NULL DEFAULT '{}';

-- More than one port is worth surfacing on its own: an inventory that knows
-- about 22 and not 2222 leaves a documented, unmonitored way in.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS ssh_ports  INT[] NOT NULL DEFAULT '{}';

-- 'unreviewed' | 'enrolled' | 'ignored'
--
-- Without somewhere to put "we know, and it is deliberate", a coverage view
-- accumulates permanent noise until people stop reading it — which costs more
-- than never having built it. Ignoring is a decision, so it records who made it
-- and why, and it is visible rather than a deletion.
ALTER TABLE agents ADD COLUMN IF NOT EXISTS enrollment_state TEXT NOT NULL DEFAULT 'unreviewed';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS review_note      TEXT NOT NULL DEFAULT '';
ALTER TABLE agents ADD COLUMN IF NOT EXISTS reviewed_by      TEXT;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS reviewed_at      TIMESTAMPTZ;
ALTER TABLE agents ADD COLUMN IF NOT EXISTS first_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now();

-- Agents that existed before this column did would otherwise claim to have been
-- first seen at the moment of the upgrade, which reads as "first seen after it
-- was last seen". Their real first sighting is unknown; the earliest defensible
-- answer is when they were last heard from.
UPDATE agents SET first_seen_at = last_seen_at WHERE first_seen_at > last_seen_at;

-- Discovery is read most often as "what has not been dealt with", so index the
-- filter rather than the whole table.
CREATE INDEX IF NOT EXISTS agents_unreviewed_idx
    ON agents (last_seen_at DESC)
    WHERE matched_asset = false AND enrollment_state = 'unreviewed';

-- Where an asset came from, so a reviewer can tell an entry someone vouched for
-- from one a machine proposed.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS discovered_by TEXT;
