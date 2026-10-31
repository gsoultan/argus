-- Teams: a name for a group of people, so a server can be given to all of them
-- at once.
--
-- Giving a server to five people meant five assignments, and a sixth person
-- joining meant revisiting every server the other five had. A team is assigned
-- once; joining it is the only change a new member needs.
--
-- Still explicit rather than computed from a directory: "who can reach pay-01
-- as ops" stays one query, now a union of two tables both keyed by the asset.
CREATE TABLE IF NOT EXISTS teams (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    created_by TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (length(btrim(name)) BETWEEN 1 AND 80)
);
-- Two teams that differ only in case would be two answers to "who is in Ops".
CREATE UNIQUE INDEX IF NOT EXISTS teams_name_lower_key ON teams (lower(name));

-- Lowercased on write, as asset_assignments.user_email is (see 008_email_case).
CREATE TABLE IF NOT EXISTS team_members (
    team_id    UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_email TEXT NOT NULL,
    added_by   TEXT NOT NULL DEFAULT '',
    added_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_email)
);
-- Every session start asks which teams a person is in.
CREATE INDEX IF NOT EXISTS team_members_user_idx ON team_members (user_email);
-- And by team, which is the console's question and what the cascade walks.
CREATE INDEX IF NOT EXISTS team_members_team_idx ON team_members (team_id);

-- A team's access to one asset. The same rule as asset_assignments: the
-- principals are bounded by the asset's own list at read time, so removing an
-- account from a server removes it from every team holding it.
CREATE TABLE IF NOT EXISTS asset_team_assignments (
    asset_id   UUID NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    team_id    UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    principals TEXT[] NOT NULL DEFAULT '{}',
    granted_by TEXT NOT NULL DEFAULT '',
    granted_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (asset_id, team_id)
);
CREATE INDEX IF NOT EXISTS asset_team_assignments_asset_idx ON asset_team_assignments (asset_id);
CREATE INDEX IF NOT EXISTS asset_team_assignments_team_idx ON asset_team_assignments (team_id);
