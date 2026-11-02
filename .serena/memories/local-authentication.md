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
- `cmd/argus-control/users.go` -- `argus-control users add|list|disable|enable`
  and the recovery commands `reset-mfa|set-password|set-role|sign-out`.
- `internal/control/session_gate.go` -- the cookie after the signature: account
  state, revocation, CSRF origin check, MFA-required hold.
- `web/src/components/TwoFactor.tsx` -- enrolment, with a QR code drawn
  locally (`qrcode-generator`), and the full-screen hold.

## Properties that must not regress

- **Every refusal is identical, and costs the same.** `Authenticate` verifies
  against a dummy hash when the account is missing or has no password. Without
  it a missing account returns in microseconds and a real one pays the full
  argon2 cost, and that difference alone enumerates the user list.
- **Beginning an enrolment does not enrol, and does not un-enrol.** The new
  secret goes to `totp_pending_secret`; `totp_secret` and `mfa_enrolled_at`
  are untouched until a code from the new one confirms it. The first version
  wrote over `totp_secret` and cleared `mfa_enrolled_at`, so a stolen session
  could switch the second factor off with one request. `/auth/mfa/enrol` now
  also demands the password, and the current code when one is enrolled.
- **A cookie is checked against the account on every request**
  (`session_gate.go`, `Store.SessionState`): disabled, revoked by id
  (`revoked_sessions`), or issued before `users.sessions_valid_after`, and the
  role is the account's now. Sign-out, password change, disable, MFA confirm
  and reset all revoke. Before this a cookie worked for its whole eight hours
  with the role it was minted with, whatever happened to the account.
- **An authenticator code is spent when accepted** (`users.totp_last_step`),
  so one code signs in once, not for ninety seconds.
- **Owners, admins and approvers must have a second factor** (`require_mfa`,
  default `privileged`). Without one the gate refuses everything but
  `/auth/me`, enrolment, password change and sign-out, and the console shows
  only `TwoFactorRequired`.
- **Per-account throttles** on top of the per-client ones (`throttle.go`): 10
  wrong passwords an hour from anywhere, 5 wrong codes. The password one
  exempts a browser carrying an `argus_device` cookie for that account, so a
  botnet cannot lock the account holder out; the code one has no exemption,
  because whoever is guessing already has the password. First trip is audited
  at `critical` and sent to the alert channels.
- **Recovery is on the host only**: `argus-control users reset-mfa |
  set-password | set-role | sign-out`, see `docs/admin-recovery.md`. The
  console lets an admin reset someone else's second factor, never their own,
  and never an owner's.
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

## Sign-in alerts

`internal/control/signin_alerts.go`, `internal/notify/signin.go`, operator
doc `docs/sign-in-alerts.md`. `completeSignIn` calls `announceSignIn`, so only
a sign-in that succeeded can send anything. Channels are email and Telegram
only, and only those configured (`SignInAlerts` is nil with neither).
Administrators: their own addresses for every organization; the configured
`to` list and Telegram chats only for the default one, because those are the
platform's. The person: email only.

- New device = `sign_in_devices` has no row for SHA-256(IP or IPv6 /64 +
  browser family + OS) in 90 days. Browser version is left out so an update
  is not a new device.
- `signInAlertLimiter`: one routine alert per account per 10 minutes, five of
  any kind per hour, map bounded at 10,000 keys.
- The "this wasn't me" link is an `auth.SignInReport`, single-use through the
  ticket redemption record. Its JSON fields share no name with Session,
  Ticket or the challenge, so it verifies as none of them
  (`TestAReportLinkAuthenticatesNothing`). GET only shows the page; mail
  scanners open every link. POST revokes every session and audits.
- A recovery-code sign-in skips `Alerts.SecurityEvent` when sign-in alerts
  already tell administrators, so they are not told twice.
