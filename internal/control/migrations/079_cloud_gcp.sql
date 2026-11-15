-- Google Cloud projects can be connected beside AWS accounts. A project is
-- an account here: account_id is its project id, role_arn the email of the
-- argus-read service account Argus acts as, identity_arn the same. Nothing
-- secret is stored for it either.
--
-- Only widens a check: a version from before still reads every row it knows.
ALTER TABLE cloud_accounts DROP CONSTRAINT IF EXISTS cloud_accounts_provider_check;
ALTER TABLE cloud_accounts ADD CONSTRAINT cloud_accounts_provider_check CHECK (provider IN ('aws', 'gcp'));
