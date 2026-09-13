# Local authentication

Argus holds its own accounts. There is no identity provider: OIDC was removed
in full (provider, flow, role mapping, the `idp_subject` column and the Dex
stand-in), so local accounts are the only way in.

Before this, signing in meant standing up OIDC first -- right for an enterprise
that already runs one, wrong as the price of admission for everyone else, and
the reason the product could not be logged into at all. The `users` table
already carried `idp_subject` (null for a local account) and `mfa_enrolled`, so
this finished a shape that was designed for rather than adding a new one.

## Shape

- `internal/auth/password.go` -- argon2id, PHC string format. Parameters live in
  the hash, so raising them re-hashes on next sign-in instead of invalidating
  every password.
- `internal/auth/totp.go` -- RFC 6238, stdlib only. Pinned against the RFC's
  published vectors in `totp_test.go`. Written here rather than pulled in
  because the algorithm is forty lines and fully specified, and a dependency in
  the authentication path of *this* product is worth avoiding when it is this
  small.
- `internal/control/accounts.go` -- the store. `migrations/007_local_accounts.sql`
  adds `password_hash`, `totp_secret`, `mfa_enrolled_at`, `disabled_at` and the
  `recovery_codes` table.
- `internal/control/password_routes.go` -- `/auth/password`, `/auth/mfa`,
  `/auth/mfa/enrol`, `/auth/mfa/confirm`, `/auth/password/change`.
- `web/src/components/SignIn.tsx` -- the form.
- `cmd/argus-control/users.go` -- `argus-control users add`.

## Properties that must not regress

- **Every refusal is identical, and costs the same.** `Authenticate` verifies
  against a dummy hash when the account is missing or has no password. Without
  it a missing account returns in microseconds and a real one pays the full
  argon2 cost, and that difference alone enumerates the user list.
- **Beginning an enrolment does not enrol.** The secret is stored with
  `mfa_enrolled_at` null and is not accepted at sign-in until a code confirms
  it. Otherwise starting an enrolment and walking away leaves an account whose
  second factor nobody can produce.
- **A challenge is not a session.** The token between "password accepted" and
  "code accepted" has its own type and a `purpose` field, and
  `TestAChallengeIsNotASession` asserts it cannot reach the API.
- **Recovery codes are single-use and replaced on re-enrolment**, so a lost
  device leaves nothing working. Spending one is audited at `warning`.
- **Changing a password requires the current one**, even with a valid session:
  a borrowed unlocked laptop must not lock the owner out.
- **Nothing is seeded.** A default administrator with a known password is a
  backdoor. The first account is made on the host, which is a privilege
  boundary that already exists.

## First run

`/auth/me` reports `passwordEnabled` and `accountsExist`, and the sign-in screen
renders the password form when both hold. With no accounts it shows the
`users add` command instead of a form that could only refuse. `accountsExist`
keys on `password_hash IS NOT NULL AND disabled_at IS NULL` -- a row without a
hash must not make the console claim a password will work.
