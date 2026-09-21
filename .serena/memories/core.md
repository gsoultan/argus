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
  The header badge in `web/src/components/Shell.tsx` is where that is stated
  for the console as a whole — `Local fixture` in amber when no control plane is
  configured, the control plane's own host when one has answered, and a refusal
  to guess in between. It replaced a hard-coded `northwind-prod ·
  ap-southeast-3` over a tooltip calling it the tenant and region, neither of
  which exists anywhere in the domain. This matters more than a label usually
  would because `live.orFallback` serves the fixture whenever the control plane
  cannot be reached, so a console showing invented numbers is otherwise
  indistinguishable from one showing a real fleet.

  **It shows the host, and there is deliberately no configurable deployment
  name.** Checked before deciding: the control plane has no identity to report —
  no tenant, no region, no gateway registry, `gateway_policy` is a single row
  and the only heartbeats are agents reporting their own hostname. Multi-gateway
  is many gateways to one control plane, and the console talks to the control
  plane. Same-origin is the supported shape because the session cookie depends
  on it, so the browser's host *is* the control plane's address rather than a
  stand-in for it, and a name from config would be weaker: it can be typoed,
  copied between environments, or claimed by two deployments at once.

  **And when a configured control plane stops answering, the shell says so at
  the size of the problem**, not only in the corner. `FallbackBanner` in
  `Shell.tsx` — full width, filled, naming the host, telling the operator not to
  act on anything below. Prompted by actually looking at the rose badge
  rendered: the corner said "not answering" while the page underneath announced
  "13 hosts would not record a bypass", three agents gone silent and six live
  sessions, every figure invented. A specific, alarming, actionable claim about
  a fleet that is not yours outweighs a badge. Settings had carried an
  equivalent for the *unconfigured* case for a while; the configured one is more
  dangerous, because then the operator expects real data. `dataSource()` decides
  once so the badge and the banner cannot disagree.
- **A verdict must say what it covered.** The audit page fetches the most recent
  `AUDIT_WINDOW` (500) entries, verified them, reported "Chain intact", and
  offered an evidence pack whose own doc comment called it "the whole chain" --
  with 4,046 of the dev log's 4,546 entries absent and nothing saying so. A hash
  chain checked from an arbitrary starting point proves that fragment is
  internally consistent and **nothing whatever** about what came before it, so
  the verdict was true of a window and false of the log.

  `/api/v1/audit` now returns `X-Argus-Audit-Total`, the page states its
  coverage above the verdict badges, and the pack records `totalEventsInLog`,
  `complete` and `coveredSequences`. `complete: null` means the server did not
  say and must never be written as `true`.

  **Still a window.** Paging the whole log into the browser is the follow-up;
  what changed is that a partial pack can no longer be read as a complete one.
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
- **A kernel tier that stops reporting must say so.** Four faults in
  `internal/execlog`, all fixed 2026-09-11, all the same shape: evidence
  disappearing while the record looked clean. `SESSION_LEN` sized for the old
  bare-hex ids made `Track` refuse every canonical UUID, so the tier was simply
  off; `handle_fork` inserted thread tids that `handle_exit` never deleted,
  leaking one `tracked` entry per thread until the bounded map stopped
  accepting; a full ring buffer and a backed-up consumer both discarded execs
  while the session went on claiming `fidelity: ebpf`; and an unreadable argv
  was reported as a command that took no arguments. The rule these all serve is
  already written in `995e25d3` -- eBPF fidelity asserts *every execve is in
  the file*, so anything that can lose one has to end the claim.
- **After a run of fixes to one package, get a second reader before the next
  one.** `internal/execlog` took eight patches on 2026-09-11. Three of the bugs
  were introduced by the same person fixing the others. A `/code-review` of the
  package afterwards found the most serious remaining defect -- executions
  arriving after `Detach` were counted where the fidelity decision never looked,
  so sealed recordings claimed eBPF fidelity while missing commands -- plus two
  of those three self-inflicted ones. Familiarity with a file stops being an
  advantage somewhere around the fourth consecutive change to it.
- **Kernel tests skip themselves, so a green run proves nothing.** Every test in
  `internal/execlog` skips on a kernel that cannot load the probe. Check the
  skip count, not the exit code. The working recipe is in
  [[local-ebpf-cannot-be-tested]]; a full pass is ~30 tests and 0 skips.
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
