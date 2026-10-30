# Console-only and login decisions

Direct SSH logins are a setting again since `022`: `gateway_policy.direct_ssh`
(`refuse` default / `approve` / `allow`), overridable per server in
`assets.direct_ssh` (NULL follows the deployment). Narrower than the old
`allow_native_clients` (`019`, dropped in `020`): an admitted login is always
recorded. The per-asset recording scope (`017`) was removed in `021`.

## Direct SSH (022)

- Heartbeat reply carries `direct_ssh` (effective mode) and `console_only`
  (= mode != allow, for agents older than the setting: they refuse under
  approve, the closed side). The agent reports `direct_ssh` back into
  `assets.agent_direct_ssh` and persists it to `<state>/direct-ssh`
  (read before the legacy `<state>/console-only`).
- `approve`: the daemon sends the shim `Decision{Pending, Refused, Error}`
  (an old shim fails closed on it), then asks `POST /api/v1/report/direct-login`
  and long-polls `.../{id}/wait` (25s per poll, never spooled -- `reporter.Ask`).
  Rows live in `direct_login_requests`, expire after 2 minutes, max 20 pending
  per host. Approver/admin/owner decide at `POST /api/v1/direct-logins/{id}/decision`;
  the console shows them on Access requests. Unreachable control plane,
  denial, expiry or a collector shutdown all refuse.
- A finished direct session reports `directApproval` ("approved" + id, or
  "allowed"); the control plane checks it against its own rows / setting and
  writes `session.direct_recorded` (notice) only if it holds, otherwise the
  usual critical `session.direct_detected`. Code: `internal/control/direct_login.go`,
  `internal/agent/collector.go` (`admitDirect`).

## Agent

The heartbeat reply carries `console_only` (true unless direct_ssh is allow; no longer
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

## Uninstalling the agent (`argus-agent uninstall`)

The ForceCommand hook is what makes a removed agent dangerous: left behind, it
refuses every SSH login. So the uninstall checks someone could still get in
over plain SSH first (`sshWaysIn`, from `sshd -T` and the posture key scan),
comments out only ForceCommands whose command's basename starts `argus`,
validates with `sshd -t` and `sshd -T` and restores on failure, then reloads
sshd. It flushes the spool and upload queue before the agent's own credential
is revoked (`POST /api/v1/report/deregister`, audited `agent.uninstalled`),
because nothing can ship them afterwards. Recordings, `sessions.jsonl` and the
spool are kept. The agent package's `agent-preremove.sh` refuses removal while
the hook is still there.
