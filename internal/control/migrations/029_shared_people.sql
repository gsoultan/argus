-- People shared across organizations.
--
-- A person belongs to one organization, where their password and second
-- factor live. Another organization may add the same person without issuing
-- a second credential: it gets a users row of its own (its role, its
-- disabling, its audit) that points home for the sign-in.
--
-- home_tenant is NULL for a person whose credentials are here, and the id of
-- the organization that holds them for a person who was shared in. A row with
-- home_tenant set never carries a password hash or a TOTP secret.
--
-- shareable is set by the home organization, and defaults off: nobody's
-- identity can be reused elsewhere unless where they belong allows it.
ALTER TABLE users ADD COLUMN IF NOT EXISTS home_tenant TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS shareable BOOLEAN NOT NULL DEFAULT false;
