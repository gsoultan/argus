-- The identity argus-control connects to an account as. There can be more
-- than one now (Settings › Cloud identities); each account keeps the one it
-- was connected with until someone moves it, so changing the default never
-- moves an account onto a principal its roles do not trust.
--
-- Every account connected before is on the one identity there was, which
-- the config upgrade names "main". The id refers to the config file, where
-- the identity and its secret live; nothing secret is here.
ALTER TABLE cloud_accounts ADD COLUMN IF NOT EXISTS identity TEXT NOT NULL DEFAULT 'main';
