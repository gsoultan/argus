# Console-only and login decisions

Every deployment is console-only, with no switch. Migration `019` added
`gateway_policy.allow_native_clients`; `020` dropped it. The per-asset
recording scope (`017`: `every_login` / `argus_only`) was removed in `021`
because console-only refused direct logins before the scope was consulted.

## Agent

The heartbeat reply always carries `console_only: true` (and no longer
`recording_scope`; an older agent reads its absence as every_login). The agent
persists it to `<state>/console-only` and reports what it applied as
`assets.agent_console_only` (NULL = unconfirmed).

The daemon, not the shim, decides: the shim sends `start`, the daemon answers
with a `Decision` before the shell runs. A `Direct` login under console-only
is refused (`Decision{Refused, Error}` -- the `Error` makes a shim older than
`Refused` fail closed) and reported to `/api/v1/report/login` as
`session.direct_refused` (warning). Every admitted login is recorded.
`FailOpen` does not apply to a refusal. An agent that has never heard from the
control plane does not refuse (so an install cannot lock admins out before
enrolment) but still records.

A non-refused login report can only come from an agent not following
console-only (old, misconfigured or forged): it is written as a critical
`session.direct_unrecorded` saying so. Handler: `internal/control/login_report.go`.

Version skew: an old daemon never sends a Decision; the shim waits
`decisionWait` (2s) and records. An error or a closed socket is fail-closed.

Known gap: the collector socket is 0666, so a local user can fabricate
`start` messages and with them audit entries.

## Gateway

Refuses native SSH (after the handshake, `ConsoleOnlyRefusal` on stderr),
native RDP (before the target is dialled) and its own browser SSH terminal
(`/ws/session`, 403 before the ticket is redeemed). Only the unexported
`Config.directPaths`, set by package tests, opens them. Browser RDP (rdpweb) is
the one gateway session path left. argus-gateway refuses to start without
`control.url`.
