-- Azure subscriptions can be connected beside AWS accounts and Google
-- Cloud projects. A subscription is an account here: account_id is its
-- subscription id, role_arn the path of Azure's Reader role in it (the role
-- argus-control must hold there), identity_arn the role assignment Argus
-- found. Nothing secret is stored for it either.
--
-- Only widens a check: a version from before still reads every row it knows.
ALTER TABLE cloud_accounts DROP CONSTRAINT IF EXISTS cloud_accounts_provider_check;
ALTER TABLE cloud_accounts ADD CONSTRAINT cloud_accounts_provider_check CHECK (provider IN ('aws', 'gcp', 'azure'));
