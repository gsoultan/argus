-- Personal server accounts, and direct SSH for chosen people.
--
-- A direct SSH login tells Argus the local account and the address it came
-- from, nothing else. On a shared account ("ubuntu") that cannot say who the
-- person was, so it cannot be allowed for one person and refused for another.
-- A personal account can: Argus creates one per person on the servers given to
-- their team, the account name stands for the person, and "Ana may SSH
-- directly" becomes something the agent can enforce and the record can name.
--
-- Every column defaults to off, so upgrading changes nothing until an admin
-- turns something on.

-- The person's account name on servers. Chosen once and kept, even if the
-- email changes: a renamed account would be a new account to the server, and
-- the old one's history would stop pointing at anyone. NULL until it is first
-- needed.
ALTER TABLE users ADD COLUMN IF NOT EXISTS login_name TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS users_login_name_key ON users (login_name)
    WHERE login_name IS NOT NULL;
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_login_name_check;
ALTER TABLE users ADD CONSTRAINT users_login_name_check
    CHECK (login_name IS NULL OR login_name ~ '^[a-z_][a-z0-9._-]{0,31}$');

-- Their public key, which the agent writes into their personal account only
-- where a team of theirs may SSH directly. A public key, never a private one.
ALTER TABLE users ADD COLUMN IF NOT EXISTS ssh_public_key TEXT NOT NULL DEFAULT '';

-- Who removed someone, next to when, so the list of removed people answers
-- both without a trip through the audit log.
ALTER TABLE users ADD COLUMN IF NOT EXISTS disabled_by TEXT;

-- A team assignment that gives each member an account of their own on the
-- server, optionally with sudo that asks no password (the account has none
-- to ask for), and optionally reachable by SSH straight to the server:
--   off      through the console only
--   approve  direct, held until an admin approves each login
--   allow    direct, let in and recorded
-- Whatever is chosen, an admitted login is recorded in the person's name.
ALTER TABLE asset_team_assignments ADD COLUMN IF NOT EXISTS personal_accounts BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE asset_team_assignments ADD COLUMN IF NOT EXISTS sudo BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE asset_team_assignments ADD COLUMN IF NOT EXISTS direct_ssh TEXT NOT NULL DEFAULT 'off';
ALTER TABLE asset_team_assignments DROP CONSTRAINT IF EXISTS asset_team_assignments_direct_ssh_check;
ALTER TABLE asset_team_assignments ADD CONSTRAINT asset_team_assignments_direct_ssh_check
    CHECK (direct_ssh IN ('off', 'approve', 'allow'));
-- Sudo and direct SSH only mean something on a personal account: on a shared
-- one there is nobody in particular to give them to.
ALTER TABLE asset_team_assignments DROP CONSTRAINT IF EXISTS asset_team_assignments_personal_check;
ALTER TABLE asset_team_assignments ADD CONSTRAINT asset_team_assignments_personal_check
    CHECK (personal_accounts OR (NOT sudo AND direct_ssh = 'off'));

-- What the agent last said about the personal accounts it manages: created,
-- locked, or a name already taken by an account Argus did not make. NULL is
-- an agent too old to say.
ALTER TABLE assets ADD COLUMN IF NOT EXISTS agent_personal_accounts JSONB;
