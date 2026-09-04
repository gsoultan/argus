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

## Standing rules for this repo

- **The console must never present fixture data as real.** `isLive()` exists for
  this. Any new surface that can show either has to say which it is showing.
- **A control that appears to be set must be set.** The Settings page shipped
  once with uncontrolled switches that silently reverted; that is the specific
  failure mode this product cannot have.
- **Every mutation reports both outcomes.** `run()` / `notifyError()` in
  `web/src/lib/notify.ts` exist so the guarded form is the shortest to write.
- **Private keys are mode-checked at load.** `secrets.CheckPrivate` refuses any
  key readable by group or other; every loader uses it. The CA key is the
  crown jewel — whoever reads it mints any certificate.
- **Heuristic evidence is labelled as heuristic.** Commands scraped from a PTY
  stream are never presented next to kernel-observed execve events as equals.
