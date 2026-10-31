-- Personal accounts, sudo and direct SSH for one person on one server, as
-- well as for a team.
--
-- On a server, a person's own setting replaces what their teams give them
-- there, in either direction: more (a person in Ops, who must ask, may SSH
-- straight to db-01) or less (a person in Deploy, who may SSH straight in, is
-- kept to the console on prod-01). Where a person has no setting of their own,
-- the most open of their teams applies, as before.
--
-- personal_accounts is NULL on rows that predate this, and on rows that only
-- give shared accounts: those follow the teams. TRUE gives the person their
-- own account with the sudo and direct SSH below; FALSE gives them none on
-- this server, whatever their teams say. Shared accounts (principals) are not
-- replaced: they stay the union of every row, as before.
ALTER TABLE asset_assignments ADD COLUMN IF NOT EXISTS personal_accounts BOOLEAN;
ALTER TABLE asset_assignments ADD COLUMN IF NOT EXISTS sudo BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE asset_assignments ADD COLUMN IF NOT EXISTS direct_ssh TEXT NOT NULL DEFAULT 'off';
ALTER TABLE asset_assignments DROP CONSTRAINT IF EXISTS asset_assignments_direct_ssh_check;
ALTER TABLE asset_assignments ADD CONSTRAINT asset_assignments_direct_ssh_check
    CHECK (direct_ssh IN ('off', 'approve', 'allow'));
ALTER TABLE asset_assignments DROP CONSTRAINT IF EXISTS asset_assignments_personal_check;
ALTER TABLE asset_assignments ADD CONSTRAINT asset_assignments_personal_check
    CHECK (personal_accounts IS TRUE OR (NOT sudo AND direct_ssh = 'off'));
