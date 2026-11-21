-- Monitoring: groups of checks, and who outside the fleet roles sees them
-- (checks_access.go). Admins, owners and auditors see every check, as before;
-- anyone else sees the checks they, or a team of theirs, were given, directly
-- or through the check's group. can_operate adds "check now" and pause.
--
-- Only new tables and one new nullable column, so the previous version
-- starts on this database and ignores them.

CREATE TABLE IF NOT EXISTS check_groups (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL CHECK (length(btrim(name)) BETWEEN 1 AND 60),
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Two groups that differ only in case would be one group to whoever reads them.
CREATE UNIQUE INDEX IF NOT EXISTS check_groups_name_lower_key ON check_groups (lower(name));

ALTER TABLE checks ADD COLUMN IF NOT EXISTS group_id UUID REFERENCES check_groups (id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS checks_group ON checks (group_id) WHERE group_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS check_viewers (
    id          BIGSERIAL PRIMARY KEY,
    -- What is given: one check, or every check in a group.
    check_id    UUID REFERENCES checks (id) ON DELETE CASCADE,
    group_id    UUID REFERENCES check_groups (id) ON DELETE CASCADE,
    -- Who is given it: one person, or everyone in a team.
    user_email  TEXT CHECK (user_email IS NULL OR (user_email = lower(btrim(user_email)) AND length(user_email) BETWEEN 3 AND 320)),
    team_id     UUID REFERENCES teams (id) ON DELETE CASCADE,
    -- Check now and pause, besides seeing it.
    can_operate BOOLEAN NOT NULL DEFAULT false,
    granted_by  TEXT NOT NULL,
    granted_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT check_viewers_one_target CHECK (num_nonnulls(check_id, group_id) = 1),
    CONSTRAINT check_viewers_one_holder CHECK (num_nonnulls(user_email, team_id) = 1)
);
CREATE UNIQUE INDEX IF NOT EXISTS check_viewers_once ON check_viewers (
    COALESCE(check_id::text, ''), COALESCE(group_id::text, ''), COALESCE(user_email, ''), COALESCE(team_id::text, ''));
CREATE INDEX IF NOT EXISTS check_viewers_user ON check_viewers (user_email) WHERE user_email IS NOT NULL;
CREATE INDEX IF NOT EXISTS check_viewers_team ON check_viewers (team_id) WHERE team_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS check_viewers_check ON check_viewers (check_id) WHERE check_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS check_viewers_group ON check_viewers (group_id) WHERE group_id IS NOT NULL;
