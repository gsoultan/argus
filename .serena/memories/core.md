# Argus — core memory

Commercial privileged-access management (PAM) for Linux SSH first, with Windows
RDP as a separate service. Go control plane + gateway, React console in `web/`.
Ships via GoReleaser, not Docker. An agent is required on each host — that is a
product decision, not an oversight, and the console says so out loud.

Read this file first, then only the memory a task actually needs.

| Memory | Read it when |
| :--- | :--- |
| [web-console-architecture](web-console-architecture.md) | Touching anything under `web/` |
| [replay-player-design](replay-player-design.md) | Session recording, playback, or memory questions |
| [design-system](design-system.md) | Any visual or component change |
| [pwa-and-service-worker](pwa-and-service-worker.md) | Caching, offline, auth redirects, releases |
| [gateway-policy](gateway-policy.md) | SSH channel policy, forwarding, recording guarantees |
| [browser-checks](browser-checks.md) | Playwright suite, memory ceilings, how to measure |
| [local-authentication](local-authentication.md) | Passwords, TOTP, recovery codes, first-run |
| [capacity-and-drills](capacity-and-drills.md) | Load testing, published limits, backup restore drills |
| [upgrades-and-bootstrap-auth](upgrades-and-bootstrap-auth.md) | Migrations, account identity, static tokens, multi-gateway |
| [shutdown-and-evidence](shutdown-and-evidence.md) | Any long-running loop, restarts, drains, reporting goroutines |

## Standing rules for this repo

- **The console must never present fixture data as real.** `isLive()` exists for
  this. Any new surface that can show either has to say which it is showing.
- **A control that appears to be set must be set.** The Settings page shipped
  once with uncontrolled switches that silently reverted; that is the specific
  failure mode this product cannot have.
- **Every mutation reports both outcomes.** `run()` / `notifyError()` in
  `web/src/lib/notify.ts` exist so the guarded form is the shortest to write.
- **Hash what you store, at the precision you store it.** `chainHash` formatted
  the audit timestamp with nanoseconds while TIMESTAMPTZ keeps microseconds, so
  the chain could not verify on Linux at all -- the product's core claim, broken
  on every real deployment, invisible on macOS where the clock is already
  microsecond-granular. See [gateway-policy](gateway-policy.md).
- **Private keys are mode-checked at load.** `secrets.CheckPrivate` refuses any
  key readable by group or other; every loader uses it. The CA key is the
  crown jewel — whoever reads it mints any certificate.
- **Heuristic evidence is labelled as heuristic.** Commands scraped from a PTY
  stream are never presented next to kernel-observed execve events as equals.
- **The native RDP listener authenticates nobody.** `rdp.Request` is parsed from
  an mstshash cookie and carries a principal and a target -- no person.
  `Session.User` is synthesised as `principal@hostname`, and the CredSSP
  identity is the *target* account Argus injects from the vault, not the
  operator. So that path cannot call `/report/authorize` meaningfully: there is
  no email to hold a grant and none to name in the audit chain. Elevated
  principals are refused outright there as of 2026-09-10; the browser console
  is the path that authenticates before it brokers. Anything proposing
  approvals, JIT or per-user policy for native RDP has to add identity first.

  The options were costed on 2026-09-11, and the decision is to leave it
  refused until Windows RDP is actually on the roadmap:

  - **Gateway as an NLA server** is how commercial RDP proxies do this, and it
    is blocked. Server-side NTLM needs the NT hash -- MD4 of the UTF-16
    password -- and accounts are argon2id (`internal/control/accounts.go`).
    Storing NT hashes alongside would be a password-equivalent secret in the
    database, which is a downgrade, so this needs Kerberos/AD delegation.
  - **A short one-time code in the mstshash cookie** is the practical one.
    `ParseCookie` already takes `principal:host`, and recovery codes are the
    precedent for a short high-entropy secret (80 bits, SHA-256, not argon2).
    The existing `Signer.IssueTicket` is far too long -- it is a signed token
    carrying email, scope, target and expiry -- so this needs a new opaque
    code. Verify mstsc's truncation of that field before committing to it.
  - **Out-of-band pre-authorisation** binding source IP plus target for a short
    window needs no client change and is not identity; IP is spoofable and NAT
    collides. Rejected for a product whose claim is knowing who did what.
- **An optional dependency is an interface field, so it is never assigned a nil
  pointer.** `gateway.Config.Storage` changed from `*storage.Client` to a
  one-method interface so the stranded-upload case could be tested without a
  live MinIO, and `main` went on assigning a possibly-nil `*storage.Client`.
  A nil pointer in an interface is not a nil interface: all four
  `cfg.Storage == nil` guards went dead at once, and a gateway with no
  `storage:` block logged an upload failure per session and spooled every
  recording for a retry that could never succeed. `cmd/argus-gateway`'s
  `recordingStore` exists for this, and is tested. The same trap waits for
  `Reporter`, `CA` and `Policy` the day any of them stops being concrete.
