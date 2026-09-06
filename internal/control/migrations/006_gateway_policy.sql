-- Gateway policy: the channels a brokered session may open, and the
-- guarantees the recorder must meet before one is allowed to proceed.
--
-- The gateway has always enforced these; it simply hardcoded them. Every value
-- below is the position the code already took, so applying this migration
-- changes no behaviour. What it adds is the ability for an owner or admin to
-- deliberately loosen one, with the change recorded in the audit log against
-- their name — which is the part that was missing.
--
-- One row, enforced by the primary key rather than by convention. A policy
-- table that can hold two rows eventually does, and then "the policy" becomes
-- a question of which row you read.
CREATE TABLE IF NOT EXISTS gateway_policy (
    id                              BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),

    -- SSH channel policy. False is closed.
    allow_local_forward             BOOLEAN NOT NULL DEFAULT FALSE,
    allow_remote_forward            BOOLEAN NOT NULL DEFAULT FALSE,
    allow_agent_forward             BOOLEAN NOT NULL DEFAULT FALSE,
    allow_x11_forward               BOOLEAN NOT NULL DEFAULT FALSE,

    -- Recording guarantees. True is closed: proxying SFTP as a subsystem turns
    -- a byte stream into per-file audit events, so switching it off loses
    -- evidence rather than gaining freedom.
    proxy_sftp_subsystem            BOOLEAN NOT NULL DEFAULT TRUE,
    fail_closed_on_recording_loss   BOOLEAN NOT NULL DEFAULT TRUE,
    require_ebpf_for_root           BOOLEAN NOT NULL DEFAULT TRUE,
    encrypt_recordings_separate_key BOOLEAN NOT NULL DEFAULT TRUE,

    updated_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Empty until someone changes it: the defaults are the product's position,
    -- not a decision any particular person made.
    updated_by                      TEXT NOT NULL DEFAULT ''
);

-- Seed the single row so a read never has to invent one, and so the gateway
-- gets a definite answer rather than a 404 it would have to interpret.
INSERT INTO gateway_policy (id) VALUES (TRUE) ON CONFLICT (id) DO NOTHING;
