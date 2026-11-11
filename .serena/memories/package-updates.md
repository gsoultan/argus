# Operating system package updates

The agent finds apt, dnf, yum or zypper updates on its server and installs
them when the console asks or when a tag's rule and maintenance window say so.
Agent: `internal/agent/packages*.go`. Control plane:
`internal/control/package_updates.go` (store) and `package_routes.go`
(heartbeat part, routes, audit, alerts). Migration 050. Console: the server's
Updates tab (`web/src/components/server/UpdatesTab.tsx`), the fleet card on
Servers (`PackageFleet.tsx`), Settings › Updates (`PackageUpdateSettings.tsx`),
queries in `web/src/lib/packageQueries.ts`. Live only: nothing here has a
fixture.

## How a run travels

- Everything rides the heartbeat. The agent sends `packages` (support, the
  latest report when newer than the last one delivered, the run it is
  following); the reply carries `packages` (check on/off, a check id the
  console asked for, the oldest queued/running/rebooting run). No background
  loop on the control plane.
- A run stays on every reply until the agent reports how it ended, so a lost
  reply only delays it. `sent_at` is set the first time it rides a reply; from
  then it cannot be taken back (a package manager must not be stopped half
  way).
- The install runs in a transient systemd unit (`argus-packages-<id>`) outside
  the agent's sandbox and cgroup, so it survives an agent restart. State is in
  files under the agent's state dir; the agent follows them every 2 s.
- A helper that vanished is settled by the agent: a run that asked for a
  reboot and finds a new boot id is done (rebooted); anything else is
  interrupted, the package manager is repaired (`dpkg --configure -a`,
  `apt-get -f install`, reinstall what `dpkg --audit` lists) and a fresh check
  is queued.
- The control plane calls a running/rebooting run with no update for three
  hours interrupted (`step = 'stale'`), freeing the server. A late report from
  the agent still lands on such a run and is audited as the real outcome.

- A reboot by itself is a run with mode `reboot`: nothing is installed, the
  helper reboots, and the agent that comes back settles it on the new boot id
  and looks for updates again (so "reboot needed" clears). Only offered when
  the agent's beat says `reboot: true`; the "needs a reboot" alert has subject
  `package-reboot:<agent>` and is closed by a run that rebooted.

- A release upgrade is a run with mode `release_upgrade`
  (`internal/agent/packages_release*.go`, migration 076, docs/os-upgrades.md).
  The check reports `release` (current, next, tool, reason, blockers); the
  run's `expect` is the version approved, and the helper stops if the server
  offers another. Ubuntu: do-release-upgrade with `RemoveObsoletes=no` in
  release-upgrades.d/argus.cfg for the run. Debian: rewrite Debian entries to
  the new codename, turn off third-party ones naming the old, put sources back
  if the new lists fail, then upgrade --without-new-pkgs and full-upgrade.
  RHEL family: leapp preupgrade (inhibitors stop it), leapp upgrade. Always
  reboots; the agent on the new boot reads /etc/os-release and calls it done
  only on the approved version, then `packages-release-finish` repairs the
  package manager and reinstalls the agent's tools (sshd, sftp-server, DB
  clients) that the upgrade removed. Refused where argus-control is installed;
  one server at a time; stale after 8 h, not 3.

## Rules

- One rule per tag, first match by position wins, plus exactly one rule with
  tag '' for every other server (seeded as notify). Modes off, notify,
  security, all. Windows are days + start minute + length, by the server's own
  clock (`utc_offset_minutes` from its report).
- Automatic runs: at most one per server per window (unique index on
  hostname + window_start), only with something to install, never retried in
  the same window, never caught up after a missed one. Rules are their own
  approval; `needs_approval` applies to runs asked by hand only, and the
  asker cannot approve their own.
- Holds (`gateway_policy.package_holds`) keep packages out of all and
  security runs; a package named by hand is installed anyway.
- Several servers: one batch, one at a time in the order picked; a failed or
  interrupted run cancels the ones after it (`advanceBatch`).

## Things that bit

- `apt-get update` exits 0 when every mirror fails; `aptFetchFailed` reads the
  output instead.
- `dpkg --configure -a` does not fix a half-installed package; the repair
  reinstalls what `dpkg --audit` lists.
- Console alert severity is `warning`, not `warn` (the audit log uses `warn`).
- `dedupe` sorts; a batch's order must not go through it.
- storm cannot map `SMALLINT[]`; window days are `INTEGER[]`.

Tested live on Ubuntu 24.04 (apt) and Oracle Linux 9 (dnf) in containers;
zypper only through unit tests.
