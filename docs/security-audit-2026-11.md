# Argus security audit — console, control plane, agent

Scope: session hijacking, brute force, DDoS/resource exhaustion, remote code
execution, account takeover, whether 2FA is really enforced, and admin
recovery. Code read at `main` (commit 1a0fb8d) and exercised against a real
Postgres and a built console. Fixes are in the PR from branch
`claude/project-thread-cnps5a`; the recovery guide is `docs/admin-recovery.md`.

Severity: **High** = a realistic path to someone else's access or to the
fleet. **Medium** = weakens a control that matters. **Low** = hardening.

## What was found and fixed

| # | Sev | Finding | Fix |
|---|-----|---------|-----|
| 1 | High | **A stolen session could switch 2FA off.** `POST /auth/mfa/enrol` took nothing but the cookie and wrote a fresh secret over `totp_secret` with `mfa_enrolled_at = NULL`. One request from a stolen cookie or an unlocked laptop, and the account was back to password-only. | New secret goes to `totp_pending_secret`; the one in use stays until a code from the new one confirms it. Enrolment demands the password, and the current code (or a recovery code) when one is enrolled. Confirming signs every other session out. Test: `TestAStolenSessionCannotSwitchTheSecondFactorOff`. |
| 2 | High | **Cookies outlived the account.** The cookie was an HMAC-signed blob; nothing checked it after issue. Disabling an account, demoting it, changing its password or signing out left every copy working for its full 8 h, with the role it was minted with. A stolen cookie could not be killed short of rotating the signing secret (which signs everyone out and re-keys every agent). | Every request resolves the cookie against the account (`session_gate.go`, `Store.SessionState`): disabled, per-cookie revocation (`revoked_sessions`), `sessions_valid_after`, and the role is read live. Sign-out, password change, disable, MFA confirm/reset all revoke. New `POST /auth/sessions/revoke` ("sign out everywhere"), admin `POST /users/{email}/sign-out`. Fails closed if the DB is unreachable. Tests: `TestDisablingAnAccountEndsItsSessionsAtOnce`, `TestADemotionAppliesToTheNextRequest`, `TestSigningOutRevokesACopiedCookie`, `TestChangingThePasswordSignsOutEveryOtherSession`. |
| 3 | High | **2FA was never required, and could not be set up.** The only enforcement was "if enrolled, ask". No console UI existed to enrol (the API was there, nothing called it), so in practice every account was password-only. | `require_mfa` (default `privileged`: owners, admins, approvers; `all`; `off`). An account that owes one is held to `/auth/me`, enrolment, password change and sign-out, and the console shows only the enrolment screen (QR code drawn locally, recovery codes shown once and must be copied/downloaded). Account menu: turn on / replace authenticator. Tests: `TestPrivilegedRolesMustEnrolBeforeAnythingElse`, `twoFactor.test.tsx`. Screenshots: `screenshots/security-hardening/`. |
| 4 | High | **Brute force was per-IP only.** Limits were keyed by client address (20 failures/hour). A credential-stuffing list or a botnet spreads attempts across addresses and never trips it. A 6-digit TOTP (1M space) with a 5-minute reusable challenge and a 3-step window was guessable from enough addresses. | Per-account limits across all addresses: 10 wrong passwords/hour, 5 wrong codes/hour (`account_failures_per_hour`, `account_code_failures_per_hour`). **No admin lockout:** the password limit exempts browsers carrying a signed `argus_device` cookie for that account (set on every full sign-in), so an attacker tripping it cannot lock the holder out. The code limit has no exemption, since whoever is guessing has the password. First trip is audited `critical` and sent to the alert channels (`auth.account_throttled`). Tests: `TestCodesAreLimitedPerAccountAcrossAddresses`, `TestPasswordsAreLimitedPerAccountButNotForAKnownBrowser`. |
| 5 | Medium | **A TOTP code was reusable for 90 s.** Shoulder-surfed, phished or logged, the same code signed in again within the window. | The accepted time step is stored (`totp_last_step`) with a conditional update; a code is spent once, and two concurrent sign-ins cannot both use it. Test: `TestAnAuthenticatorCodeSignsInOnce`. |
| 6 | Medium | **No CSRF defence beyond SameSite=Lax.** Lax blocks cross-*site* POSTs, not same-site ones (a sibling subdomain, anything an attacker can host under the same registrable domain), and not every browser. | `sessionGate` refuses state-changing requests whose `Origin` / `Sec-Fetch-Site` do not name this host, the configured console URL or an allowed origin. WebSocket handshakes are checked the same way before any work. Test: `TestCrossSiteWritesAreRefused`. |
| 7 | Medium | **Any signed JSON could pass as a session.** All token types share one HMAC key; a terminal ticket, an MFA challenge and the new device token all carry an email and an expiry. `VerifySession` only checked the signature and `exp`. | `VerifySession` refuses a payload with `purpose` or `jti`, or without an email. Test: `TestATicketIsNotASession`. |
| 8 | Medium | **An admin could remove every owner.** Role changes on owners were owner-only, but disable was not, so an admin could disable all owners and become the top of the tree. | Disable, reset-mfa and sign-out on an owner are owner-only (`mayManage`). Nobody can reset their own second factor from a session. Test: `TestAnAdminCannotDisableOrResetAnOwner`. |
| 9 | Medium | **`argus-control users add` silently overwrote an existing account's password and role**, audited as "created". A quiet way to take over an account from the host, with its sessions and second factor left intact. | Refuses an address that already has an account and points at the recovery commands, which each do one named thing, revoke sessions, and audit at `warning` under `<user>@host`. |
| 10 | Medium | **No admin recovery path.** Losing the last admin's phone meant hand-written SQL against `users`, with no audit entry and no session revocation. | `argus-control users reset-mfa / set-password / set-role / sign-out` on the host, plus console reset/sign-out for other people. `docs/admin-recovery.md` covers every scenario, with the rule that recovery never happens over the network. |
| 11 | Low | **HTTP server had no idle timeout or header cap**; only `ReadHeaderTimeout`. Idle keep-alives could be piled up until descriptors ran out. | `IdleTimeout: 2m`, `MaxHeaderBytes: 64 KiB`, and a 30 s read deadline on console request bodies (agents' uploads excluded). No global read/write timeout, on purpose: terminals, desktops and tunnels live for hours. |
| 12 | Low | **Postgres literal quoting depended on `standard_conforming_strings`.** `quoteLiteral` doubled quotes only; with the setting off, a backslash in an owner email would end the literal early, in a statement run as superuser by the pg-admin helper. | `E'...'` with backslashes and quotes both escaped, valid under either setting. Test: `TestQuoteLiteralSurvivesBackslashes`. |
| 13 | Low | Recovery-code sign-ins and second-factor resets were audited but reached nobody. | Both now go to the alert channels (`Dispatcher.SecurityEvent`). |

## What was checked and is sound

- **Session cookie**: HttpOnly, Secure when TLS or `behind_tls_proxy`, SameSite=Lax, HMAC-SHA256 with a ≥32-char secret, placeholder secrets refused at start. HSTS, nosniff, DENY framing, strict CSP on the API.
- **WebSockets** (terminal, files, desktop, postgres, shadow, terminate): each needs a single-use, 60 s, HMAC-signed ticket bound to target+principal or session id, redeemed atomically in the DB across replicas; Origin checked against allowed origins. Shadow and terminate have distinct scopes that cannot be swapped.
- **Agent command path (RCE)**: the control plane does not send commands; it sends an ed25519-signed *grant* naming host, service, principal, person and expiry. The agent verifies against a key it **pinned on first contact** (`grant_pin.go`), rejects other hosts, expired or far-future grants, and replays (bounded set). A compromised control plane TLS path cannot forge a grant without the pinned key. Shells start as the named account via `systemd-run --uid` or `setuid`, with a fixed environment and PATH. File ops run as the account (`file-op` helper), never root. No `sh -c` with user-supplied strings anywhere: the only `-c` launchers take fixed scripts with the shell path as `$0`.
- **Agent → control plane**: per-host credential (hashed in DB), optional mTLS, hostname bound to credential, report bodies capped at 1 MiB (unknown fields logged, not fatal), recording uploads capped at `MaxRecordingUpload`, exit codes range-checked.
- **SQL console**: statements go through the agent to Postgres as the person's *own* role (`argus_<level>_<db>` grants), with `statement_timeout`, `idle_in_transaction_session_timeout`, max rows, idle and max session caps from policy; query text capped (`tunnel.MaxSQL`). Role and database names are validated identifiers; a bad one panics the helper rather than quoting.
- **Deserialisation**: `encoding/json` only, `DisallowUnknownFields` on console input, no gob/yaml from the network.
- **Password handling**: argon2id, dummy hash on missing account (timing-equal refusals), one error message for every failure, 12-char minimum, password required to change password.
- **Static dev tokens**: refused at start if any account exists.
- **First-run setup**: one-time 160-bit token written to a root-only file, constant-time compared, wrong token charged as a failure, closes forever on first account, single-account under a lock.
- **Enrollment tokens**: throttled like logins, bound to hostname, single-use, expiring.
- **Direct SSH through the agent**: the agent's shim runs *after* sshd authentication (ForceCommand), so password guessing against sshd is sshd's and the host's to limit (fail2ban / `MaxAuthTries`); the shim can only hold, record, or refuse an already-authenticated login. Approval decisions are admin-only and audited.
- **Last-admin rule**: both console and CLI refuse to disable or demote the last active admin/owner, inside a transaction with the row locked.
- **Rate-limiter tables** are LRU-bounded (50k keys) so attacker-chosen keys cannot exhaust memory; `X-Forwarded-For` is honoured only from `trusted_proxies`.

## Not fixed in this pass (recommendations)

1. **Argon2 parameters under load**: each failed password costs a full argon2id hash. Per-IP and per-account limits bound this, but a wide botnet spraying *different* accounts still gets 20 hashes/IP/hour. Consider a global ceiling on concurrent hashes (a semaphore of ~CPU count) so a login flood degrades sign-in latency rather than the whole control plane.
2. **Session binding to client**: cookies are not bound to IP or TLS session. A stolen cookie now dies on sign-out/revocation, but not on its own. Logging the first client address per session id and alerting on a change would catch theft faster. (Binding hard to IP breaks mobile users.)
3. **Account enumeration via the people page** is admin-only, fine. The `users add` CLI on the host accepts a piped password; document that `HISTFILE` matters.
4. **Approval links in alerts** are plain console URLs; nothing acts on a click without a session, so there is nothing to guess.
5. **Replay set on agents** is 4096 live grants; a control plane that mints >4096 concurrent sessions to one host refuses the rest. Fine for the published limits.
6. **QUIC tunnel**: not reviewed in depth here; the HTTPS fallback was. Worth a focused pass on `admitQUIC` for pre-auth allocation.

## Operating guidance

- Leave `require_mfa` at `privileged` or set `all`. `off` logs a warning at start.
- Set `alerts` so `auth.account_throttled`, recovery-code sign-ins and MFA resets reach a phone.
- Keep two owners enrolled. Recovery is `docs/admin-recovery.md`, on the host, never in the console.
- Behind a tunnel or proxy, set `rate_limit.trusted_proxies`, or every person shares one limit.
