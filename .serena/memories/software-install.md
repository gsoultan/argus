# Installing and removing software

Install and remove packages on a Linux server from the Installed segment of
What runs here (Apps tab). Design: /mnt/project-files/plans/app-install/
(design.md, mockups). Shipped in order: PR1 Linux (this), then the software
library with scripts, Android, Windows, many servers at once.

Agent: `internal/agent/software*.go`. Shared: `internal/tunnel/software.go`
(grant, validation, result types). Control: `internal/control/software_routes.go`
plus install/remove fields on package runs in `package_updates.go`. Migration
056. Console: `web/src/components/server/InstalledSoftware.tsx`,
`web/src/lib/software{Api,Queries}.ts`.

## How it works

- Installed list, search and preview are synchronous questions over the
  tunnel (`software` service, frame 'z'), like dbdiscover. The agent runs
  `argus-agent software-query` as root in a transient unit
  (`systemd-run --pipe --wait`), JSON on stdin.
- An install or removal is a package run with mode `install`/`remove`: it
  rides the heartbeat, one run per server (updates included), approval by the
  update rule's `needs_approval`, approve/deny/cancel reuse the package-run
  routes.
- What is approved is a preview (`software_previews`, 15 minutes). The run
  carries the preview's hash in `expect`; the agent simulates again and fails
  without changing anything when the hash differs.
- Protected (never removable): the agent, package managers, systemd, libc,
  sudo, bash, coreutils, openssh, ca-certificates, the running kernel,
  dpkg Essential/required, dnf protected.d. An install that would remove
  anything is blocked. A removal takes the packages only it needed, never a
  blanket autoremove (orphans that existed before are left alone).
- Purge is apt only and never for keeps-data packages (databases, brokers,
  docker).
- Services: started by default ("start services" switch). Debian is kept
  from starting them with a policy-rc.d (exit 101) when off; RHEL/SUSE are
  enabled with `systemctl enable --now` when on.
- Undo (`POST /package-runs/{id}/undo`) is a preview: removing what an
  install newly added from its asked items, or reinstalling what a removal
  took at its old version.
- Audit: `software.install_requested`, `software.remove_requested`; the end
  is `packages.run_finished/failed` with "The install on ...".

## Decisions taken without an answer (the user never picked)

Start services: yes. Production approval: the update rule's needs_approval.
Linux: the server's own repositories only. Tab name stays "What runs here".
Admin/owner only for now (team right deferred). "Hold at this version" not
in PR1.

## Things that bit

- dnf's `installed()` keys by name.arch; diff before/after on
  `installedList` (name and name:arch).
- TanStack Query v5 passes a second argument to `mutationFn`; wrap API calls
  so tests can assert the exact arguments.
- Test helpers `contains` and `hasString` already exist in the agent package.
