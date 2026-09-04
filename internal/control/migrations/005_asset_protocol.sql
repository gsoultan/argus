-- Which protocol reaches an asset.
--
-- The gateway inventory has carried this since Remote Desktop landed; the
-- control plane inferred nothing and counted every asset as though an agent
-- could be installed on it. That made coverage wrong in a specific and
-- misleading way: a Windows host shows as an unmonitored gap an operator is
-- expected to close, when in fact no agent exists for it yet. Reporting a gap
-- nobody can act on trains people to ignore the ones they can.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS protocol TEXT NOT NULL DEFAULT 'ssh';

-- Existing rows are SSH, which is what the default records. Sessions already
-- carry a protocol, so anything that has only ever been reached over RDP can be
-- corrected from its own history rather than guessed at.
UPDATE assets a SET protocol = 'rdp'
WHERE a.protocol = 'ssh'
  AND EXISTS (SELECT 1 FROM sessions s WHERE s.asset_hostname = a.hostname AND s.protocol = 'rdp')
  AND NOT EXISTS (SELECT 1 FROM sessions s WHERE s.asset_hostname = a.hostname AND s.protocol <> 'rdp');

CREATE INDEX IF NOT EXISTS assets_protocol_idx ON assets (protocol);
