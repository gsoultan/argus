# Upgrades, account identity, and the bootstrap token

## Static console tokens are the sharpest edge in this codebase

`user_tokens:` in the control plane config maps a bearer string to an email. It
bypasses the password, the second factor and the role. It was once disabled only
when OIDC was configured -- the wrong test the moment Argus grew its own
accounts, because a password+MFA deployment still honoured it and handed out
`Role: "admin"` regardless of whose address the token named. OIDC has since
been removed entirely, so local accounts are the only thing the gate can ask
about.

Now: the role comes from the named account, and `cmd/argus-control/main.go`
**refuses to start** when `user_tokens` is set alongside any local
account. Refuses rather than ignores -- a deployment that thinks its dev token
works will use it.

The only supported case is bootstrap: no accounts at all. It warns.

**If you add another authentication path, revisit `API.authenticate`.** The
pattern to watch for is a guard that names one mechanism instead of asking the
real question ("is there a real way in?"). `a.oidc != nil` was exactly that
guard, and removing OIDC deleted it rather than fixing it -- the question is now
carried by `StaticTokensDisabled` alone.

## Email identity: store and compare the same string

`users.email` has a case-sensitive UNIQUE constraint. Every lookup in
`accounts.go` matches `lower(email)`. Those disagreed, so `Alice@corp.com` and
`alice@corp.com` were two accounts answering one login, and `SetPassword` /
`BeginMFAEnrolment` (no LIMIT on `WHERE lower(email) = ...`) wrote to both.

`008_email_case.sql` normalises and moves uniqueness onto `lower(email)`. It
**refuses** on pre-existing case-collisions rather than merging, because that
means choosing which role, password and audit history survive.

`normaliseEmail` is the one form addresses are stored in. Use it on any new
write path.

## Migrations must be tested against populated data

Every migration had only ever run on an empty schema -- the one situation no
customer is in. `migrate()` is split into `migrationNames()` +
`applyMigrations()` so a subset can be applied.

`internal/control/migration_test.go` stops at version k, seeds through the
schema as it existed then, applies the rest, and checks rows survived, the
audit chain still verifies **to the same head**, and re-running is a no-op.
Runs for every boundary. A second test asserts a fresh install and an upgraded
database reach byte-identical schemas.

Seed with the *oldest* column set: later migrations add columns with defaults,
so 001-era inserts stay valid at every version. `sessions.id` has no default --
the gateway mints it, so seeds must too.

Needs `ARGUS_TEST_DATABASE_URL` and a role with CREATEDB. It fails rather than
skips if it cannot create a database.

**Test cleanup that uses a closed pool fails silently.** `scratchDB` deferred
`pool.Close()` then reused the pool from `t.Cleanup` with the error discarded;
69 scratch databases accumulated before `CREATE DATABASE` blocked long enough
to time other tests out. Each admin statement now opens its own connection.

## Two gateways

Verified 2026-09-07. Two instances, one control plane, 100 concurrent sessions
each: 200/200, every session id distinct, audit chain intact after concurrent
writes from both. Pins come from the control plane; a terminal ticket redeemed
on one gateway is refused by the other with 401. Config:
`dev/argus2.yaml.example`. See [[capacity-and-drills]].

## The agent makes the strongest claim in the product

eBPF fidelity means "every execve is in this file". `activeSession.recordExec`
used to log-and-drop a failed write while the session kept the claim. It now
degrades to `pty`. `internal/agent` had no coverage of the session path at all,
which is how it survived -- the same bug shape as the gateway's `relay()`.

## ./... includes web/node_modules

Go does not skip `node_modules`, and `flatted` ships Go source. `check.sh` and
CI list packages via `go list ./... | grep -v /node_modules/`.

## Updating an agent from the console (2026-11)

Server page, **Agent** card. The request lives on `agent_credentials.update_*`
(migration 034) and rides every heartbeat reply until the agent reports an
outcome for that `update_id`; then it is cleared and audited once
(`recordAgentUpdate` clears with `WHERE update_id = $id`). The agent's own
account (`update` on the heartbeat: supported/reason, state, sessions,
message) is kept on `agents.update_status`. Code: `internal/agent/update.go`
(Updater), `update_linux.go` (platform + the `update-apply` helper),
`internal/control/agent_update.go`, `web/src/components/AgentUpdate.tsx`.

What made the old upgrade drop things, and what replaced each:

- **The restart took every session.** The update waits for `Busy()` = shim
  connections + open non-ping tunnel streams to be 0, then holds new sessions
  (collector replies `restarting`, tunnel gate refuses) and checks again.
- **Logins in the restart window were refused.** The shim retries the socket
  for 5s always (covers a hand `systemctl restart`), and for 75s when told
  `restarting` or when `/run/argus/updating` is fresh. The unit keeps
  `/run/argus` across restarts (`RuntimeDirectoryPreserve=restart`) so the
  marker survives.
- **SSH logins went on unrecorded after any agent restart**, silently. The
  shim buffers (4 MB) and reconnects to the next agent, which records the rest
  as a new session whose start says which one it continues. A login the new
  agent will not take says so on the terminal and in the auth log.
- **No way back.** The helper runs in its own transient unit (outside the
  sandbox and the agent's cgroup) from a copy of the binary, installs with
  `ARGUS_AGENT_UPDATING=1` (postinstall then skips its own restart), restarts,
  and waits for `ready.json` (version, listening, tunnel). Not ready in 90s, or
  3 automatic restarts by systemd, reinstalls the old package (or the copied
  binary).

Drill (real systemd container, packages from nfpm, release mirror over HTTP):
an update waited out a console terminal and an SSH login, restarted in 1s,
92 logins during it, 0 refused; "now" kept an SSH login connected with its
recording continuing; a version that exits at once went back in ~8s.

Traps found in the drill: the helper must date what it writes to
`update.json` -- undated, the new agent took it for a lost helper and wrote
"failed" over the helper's own result. And the new agent must watch the file
(`Updater.Watch`), or "done" reaches the console only at the next heartbeat.

### Windows (2026-11)

`update_windows.go` (platform: `check`/`restartCheck` need elevated + running
as the `argus-agent` service + the service's BinaryPathName being the running
exe, cached 5 min) and `update_service.go` (`ServiceApplier`, no build tag so
Linux CI tests it with a fake service manager). No package: the release file
is `argus_<v>_windows_<arch>.zip`; `prepare` sees the `archiveInstall`
interface, unpacks `argus-agent.exe` before anything stops, and skips the
running version's archive (the copied binary is the whole install). The helper
is a detached copy of the exe (`DETACHED_PROCESS|CREATE_NEW_PROCESS_GROUP`,
breakaway from any job when allowed), logging to
`C:\ProgramData\Argus\update-helper.log`. It renames the running exe to
`.old` and moves the new one in while the old agent still serves, then
SCM-stops (killing the process after 90s) and starts. Ready check is the
shared `awaitAgentReady`; a crash loop is the service's process id changing or
going to 0, three times (recovery restarts after 5s, 5s, 30s). Rollback stops
first, then swaps the previous binary back. CI job `windows-agent-update`
drills update, rollback and restart on a real service.

### Restarting an agent from the console

A restart is an update request with `agent_credentials.update_restart` set
(migration 044) and `update_version` = the version the agent runs; the
heartbeat reply carries `restart: true`. The agent (`Updater.Handle` with
`req.Restart`) skips the download, waits and holds like an update, and hands
the helper a plan with `restart: true`, which runs `Applier.applyRestart`:
`systemctl restart`, then the same `ready.json` check. Nothing to go back to,
so not ready in 90s is `failed`. The agent advertises `restartable` /
`restart_reason` on its update status; an agent without either is too old,
and the console says to update it once. A restart is refused while an update
waits (the update restarts it anyway); an update replaces a waiting restart
(`requestAgent` keeps the id only for the same kind and version). The fleet
does not count a restart as `updating`. Audit: `agent.restart_requested`,
`agent.restarted`, `agent.restart_failed`, `agent.restart_cancelled`.

### Newest release, Update all, automatic updates

`internal/control/agent_releases.go` (`ReleaseWatcher`) reads GitHub's release
list every 6h and keeps the highest release tagged `v<version>` that carries
`checksums.txt` and `argus-agent_<v>_linux_*`. A tag without the `v` (the agent
downloads from `<url>/v<version>/`) or a release whose build left no files is
skipped. `API.agentTarget()` offers the newer of that and the control plane's
own version. A mirror (`agent_releases_url`) is checked only when
`agent_releases_api` names its own list. `agent_release_check: false` turns the
check off.

`internal/control/agent_fleet.go`: `GET/POST /api/v1/agents/updates`,
`PUT /api/v1/agents/updates/automatic`. The switch is
`gateway_policy.agent_auto_update` (migration 041). With it on,
`heartbeatUpdate` asks an agent that is behind (`autoUpdate`, requested by
"automatic updates"). It never retries a target whose last status here is
`rolled_back`, `failed` or `cancelled`; that state is `held`, and naming the
host in the POST retries it. Console: `web/src/components/AgentFleet.tsx` on
Servers.

Store queries in `agent_update.go` used `s.pool` (the default organization)
until this change. They must use `s.db(ctx)`, like every per-organization
query.
