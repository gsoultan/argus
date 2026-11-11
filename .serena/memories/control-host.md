# The control server's own helper

argus-control looks after the server it runs on through the agent, shipped in
the argus-control package as `argus-control-host` and enrolled by argus-control
itself. Server: `internal/control/control_host.go` (loop, asset marking,
audit), wired in `cmd/argus-control/control_host.go`. Helper:
`argus-agent enroll-local` in `cmd/argus-agent/enroll_local*.go`. Units:
`packaging/systemd/argus-control-host.service` and
`argus-control-host-enroll.path`. Migration 076. Console:
`web/src/components/server/controlHost.tsx`.

## Why the agent and not a root helper in argus-control

The layering rule forbids argus-control linking agent code, so a helper inside
argus-control would have been a second copy of log cleanup, package updates,
disk growing and the recorded terminal, all as root. Shipping the agent binary
under another name reuses every plan, re-check, recording and audit entry
unchanged. The person asked for "without adding an agent": nothing is installed
or enrolled by hand, which is what that meant.

## How it travels

- While `os.Hostname()` has no live credential, argus-control mints a 24 h
  single-use token marked `control_host` (actor `argus-control`, audited) and
  writes `<data dir>/host/enroll.json` 0600 by rename. Kept while it has more
  than an hour left; removed once a live credential exists. Never minted while
  the hostname has any live credential (an argus-agent enrolled by hand).
- The path unit starts the helper; `ExecStartPre=enroll-local` reads the file
  as hostile input (O_NOFOLLOW, regular, not group/other writable, 16 KiB,
  https only, its own hostname not the file's) and calls `agent.Enroll`.
- `postEnroll` sees `control_host` on the token, carries it to the credential,
  and `MarkControlHostAsset` marks the matching asset (by hostname or
  agent_hostname, archived ones revived) or adds one (127.0.0.1, no
  principals, group `argus`).
- URL: loopback for a wildcard `listen`, the bound address otherwise. Needs
  TLS served by this process with a pin, and no `client_ca_file`; any other
  layout logs the reason at start and does nothing.

## What differs from an ordinary server

- No sshd hook, ever: `install` is never run. Plain SSH is the way back in
  when Argus is down, and the console says so (Settings tab, header badge
  title). Its bypass posture honestly reads open.
- Agent update and restart refused (`ErrControlHostAgent`,
  `controlHostUpdateStatus` overrides the status in the card and the fleet):
  the agent's update-apply would install the argus-agent package and restart
  a unit that is not there. It moves with argus-control.
- `DELETE /api/v1/assets/{id}` refuses it (409, `ErrControlHostAsset`).
  `control_host: false` turns the loop off.
- If the argus-agent package is installed too, the unit stands down
  (`ConditionPathExists=!.../argus-agent.service`) and the agent takes over
  the same `/var/lib/argus` enrollment. Known gap: that credential stays
  marked control_host, so its updates are still refused from the console.

Major OS release upgrades for this server come through the same helper once the
agent supports them (see package-updates).

## Guards added in the agent for this server

- `protectedService` never restarts `argus-control*` after an update run, and
  `argus-control` is a protected package (software removal).
- Log cleanup never touches `/var/lib/argus-control/`,
  `/var/lib/argus-control-update/` or `/var/backups/argus/`.
