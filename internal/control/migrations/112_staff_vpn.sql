-- The staff VPN (wg_staff.go): a WireGuard network of shape 'staff' whose one
-- member is the hub and whose other peers are people's devices. New tables
-- and new columns with defaults, so an older version ignores them; it does
-- not know the shape, and sends a staff hub nothing it would not send a hub.
ALTER TABLE wg_networks DROP CONSTRAINT IF EXISTS wg_networks_shape_check;
ALTER TABLE wg_networks ADD CONSTRAINT wg_networks_shape_check CHECK (shape IN ('mesh', 'hub', 'staff'));
-- What a device sends through the hub when its person has no override:
-- company (the ranges their teams reach) or full (everything).
ALTER TABLE wg_networks ADD COLUMN IF NOT EXISTS staff_default TEXT NOT NULL DEFAULT 'company'
    CHECK (staff_default IN ('company', 'full'));
-- How long a device profile lasts before its person renews it.
ALTER TABLE wg_networks ADD COLUMN IF NOT EXISTS staff_days INTEGER NOT NULL DEFAULT 90
    CHECK (staff_days BETWEEN 1 AND 365);
-- Whether making and renewing a profile takes an authenticator code.
ALTER TABLE wg_networks ADD COLUMN IF NOT EXISTS staff_mfa BOOLEAN NOT NULL DEFAULT true;

-- What a team reaches through a staff VPN, and whether SSH and RDP straight
-- to it are allowed (not recorded) or refused (logins go through Argus).
CREATE TABLE IF NOT EXISTS wg_staff_grants (
    network_id  UUID NOT NULL REFERENCES wg_networks(id) ON DELETE CASCADE,
    team_id     UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    ranges      TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(ranges) <= 32),
    logins      BOOLEAN NOT NULL DEFAULT false,
    updated_by  TEXT NOT NULL DEFAULT '',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (network_id, team_id)
);
CREATE INDEX IF NOT EXISTS wg_staff_grants_team_id_idx ON wg_staff_grants (team_id);

-- A person's own setting, over the network's default.
CREATE TABLE IF NOT EXISTS wg_staff_modes (
    network_id  UUID NOT NULL REFERENCES wg_networks(id) ON DELETE CASCADE,
    user_email  TEXT NOT NULL CHECK (length(user_email) BETWEEN 3 AND 320),
    mode        TEXT NOT NULL CHECK (mode IN ('company', 'full')),
    set_by      TEXT NOT NULL DEFAULT '',
    set_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (network_id, user_email)
);

-- A device profile. Its private key was handed to its person once and never
-- kept; a revoked device is deleted (the audit log keeps the record).
CREATE TABLE IF NOT EXISTS wg_staff_devices (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    network_id  UUID NOT NULL REFERENCES wg_networks(id) ON DELETE CASCADE,
    user_email  TEXT NOT NULL CHECK (length(user_email) BETWEEN 3 AND 320),
    name        TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    public_key  TEXT NOT NULL CHECK (length(public_key) = 44),
    address     TEXT NOT NULL CHECK (length(address) <= 15),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    CONSTRAINT uq_wg_staff_devices_public_key UNIQUE (public_key),
    CONSTRAINT uq_wg_staff_devices_address UNIQUE (address)
);
CREATE INDEX IF NOT EXISTS wg_staff_devices_network_id_idx ON wg_staff_devices (network_id);
CREATE INDEX IF NOT EXISTS wg_staff_devices_user_email_idx ON wg_staff_devices (user_email);
