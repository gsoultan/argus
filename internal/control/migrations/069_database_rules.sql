-- Giving access to many databases at once, and to the ones added later.
-- See database_bulk.go.
--
-- A rule says: on this server, every database (or every broker) added from
-- now on is given to this team or person at this level. It is applied when a
-- database is added, by writing an ordinary database_assignments row, so
-- deciding whether a session opens still reads only database_assignments.
--
-- database_assignments.rule_id names the rule that wrote a row, so taking the
-- rule away can take away what it gave. A row an admin then sets by hand is
-- theirs, and loses its rule_id.
--
-- Only a new table and an added nullable column: nothing to rewrite on a
-- populated database.

CREATE TABLE IF NOT EXISTS database_access_rules (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    asset_id    UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    team_id     UUID REFERENCES teams(id) ON DELETE CASCADE,
    user_email  TEXT,
    kind        TEXT NOT NULL,
    level       TEXT NOT NULL,
    approval    TEXT NOT NULL DEFAULT 'label',
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT database_access_rules_who_check CHECK ((team_id IS NULL) <> (user_email IS NULL)),
    CONSTRAINT database_access_rules_kind_check CHECK (kind IN ('database', 'broker')),
    CONSTRAINT database_access_rules_level_check CHECK (level IN ('read', 'write', 'owner', 'dba')),
    CONSTRAINT database_access_rules_approval_check CHECK (approval IN ('label', 'always', 'never'))
);
CREATE INDEX IF NOT EXISTS database_access_rules_asset_idx ON database_access_rules (asset_id);
CREATE UNIQUE INDEX IF NOT EXISTS database_access_rules_team_key
    ON database_access_rules (asset_id, kind, team_id) WHERE team_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS database_access_rules_user_key
    ON database_access_rules (asset_id, kind, user_email) WHERE user_email IS NOT NULL;

ALTER TABLE database_assignments ADD COLUMN IF NOT EXISTS rule_id UUID;
ALTER TABLE database_assignments DROP CONSTRAINT IF EXISTS database_assignments_rule_id_fkey;
ALTER TABLE database_assignments ADD CONSTRAINT database_assignments_rule_id_fkey
    FOREIGN KEY (rule_id) REFERENCES database_access_rules(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS database_assignments_rule_idx ON database_assignments (rule_id) WHERE rule_id IS NOT NULL;
