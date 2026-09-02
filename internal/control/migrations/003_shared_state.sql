-- Shared state for running more than one gateway.
--
-- Both tables replace an in-memory map that was correct for a single instance
-- and quietly wrong for two. That failure mode is the dangerous kind: nothing
-- errors, the control simply stops applying.

-- Redeemed terminal tickets.
--
-- A ticket burned on gateway A must be dead on gateway B. Held in a map, it was
-- not: "single use" degraded to "single use per gateway", which is an auth bug
-- that only appears once you scale out and succeed silently until then.
CREATE TABLE IF NOT EXISTS redeemed_tickets (
    id          TEXT PRIMARY KEY,
    email       TEXT NOT NULL DEFAULT '',
    target      TEXT NOT NULL DEFAULT '',
    principal   TEXT NOT NULL DEFAULT '',
    redeemed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Rows are swept once the ticket could no longer be valid anyway, so this
    -- table stays small rather than growing without bound on a key an
    -- attacker can influence.
    expires_at  TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS redeemed_tickets_expiry_idx ON redeemed_tickets (expires_at);

-- Pinned target host keys.
--
-- Argus terminates SSH, so it is responsible for verifying the target. With
-- per-gateway JSON files, a second gateway starts with no pins at all: under
-- trust-on-first-use it silently accepts a host the first gateway would refuse.
-- The strongest control in the product degraded to nothing on the second node.
CREATE TABLE IF NOT EXISTS host_key_pins (
    host        TEXT PRIMARY KEY,
    fingerprint TEXT NOT NULL,
    key_type    TEXT NOT NULL DEFAULT '',
    pinned_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Who vouched for it: "tofu" when accepted on first contact, otherwise an
    -- operator identity. A pin nobody can account for is worth noticing.
    pinned_by   TEXT NOT NULL DEFAULT ''
);
