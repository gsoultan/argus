-- VPN links on each server, as its agent last reported them (networks.go):
-- WireGuard interfaces and their peers (public keys, endpoints, allowed IPs,
-- last handshake, byte counters) and each cloudflared's readiness. Never a
-- private or preshared key: the agent does not read them.
--
-- NULL is "not reported", from an agent older than VPN discovery. The
-- previous version starts on this database and ignores the column.

ALTER TABLE agents ADD COLUMN IF NOT EXISTS vpn JSONB
    CHECK (vpn IS NULL OR octet_length(vpn::text) <= 400000);
