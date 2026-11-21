-- argus:breaking: authenticator secrets are sealed, and a version from before cannot read them
-- Authenticator (TOTP) secrets are sealed with AES-256-GCM under a key derived
-- from signing_secret (internal/control/mfa_seal.go). The column keeps its
-- type: a sealed value starts with 'sealed:v1:', and one without the prefix is
-- a secret from before, which still verifies until the control plane seals it.
--
-- Nothing here changes a row. The control plane seals the stored secrets once
-- it has been serving for a while, so an update that is rolled back at once
-- leaves them readable. After that, a version from before this one reads
-- every sealed secret as a wrong one and nobody's codes work: go back by
-- restoring the backup taken before the update, which holds them as they were,
-- or reset each person's second factor.
COMMENT ON COLUMN users.totp_secret IS
    'Authenticator secret: sealed:v1:<base64 nonce+ciphertext>, AES-256-GCM under a key derived from signing_secret; an unprefixed value is from before sealing';
COMMENT ON COLUMN users.totp_pending_secret IS
    'Unconfirmed authenticator secret, stored as totp_secret is';
