-- Site links (wg_networks.go): the private ranges behind a member, such as
-- an office LAN or a cloud VPC, that it routes for the rest of the network.
ALTER TABLE wg_members ADD COLUMN IF NOT EXISTS routes TEXT[] NOT NULL DEFAULT '{}'
    CHECK (cardinality(routes) <= 16);
