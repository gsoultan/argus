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

## Scripts (PR2): the Software library

- Settings › Software (`web/src/components/SoftwareLibrary.tsx`); the Install
  drawer has a Repositories | Script switch (`server/ScriptInstall.tsx`).
- Tables `software_scripts`, `software_script_versions` (migration 057).
  Every save is a new version with a SHA-256 per part (install, uninstall,
  check). Pasted text or an uploaded .sh/.ps1, normalized by
  `NormalizeScript` (BOM, UTF-16, CRLF, no binaries, 256 KB).
- Checks: `internal/control/script_check.go`, a data table of regex rules
  (blocked / warning / note), comment lines skipped, plus `bash -n` and
  ShellCheck when installed. Blocked cannot be waived; each warning needs a
  reviewer's reason. A blocked version is saved but cannot be reviewed.
- Review: by someone other than the author; with one admin only, the author
  types "review my own script" and it is marked self-reviewed. Approval is an
  ed25519 signature (`tunnel.SignScript`, context "argus script v1.") with
  the grant key; the agent verifies against its pinned grant-key.pub and the
  part's hash before running anything.
- Runs are package runs with mode `script` (heartbeat, one per server,
  update-rule approval). A version must finish once on a server without a
  production/prod/prd tag (`tried_on`) before it runs on production.
- Secret parameters: sealed with AES-GCM in `package_runs.script_secrets`,
  masked in the log, wiped when the run ends. Values reach the script as
  `ARGUS_PARAM_<NAME>` through an EnvironmentFile, never in its text.
- Agent (`internal/agent/software_script.go`) runs `bash -euo pipefail` in a
  transient systemd unit with Argus's state dir inaccessible, then the check
  script, and reports packages, services, listening ports and folders under
  /opt and /usr/local that changed.
- Windows scripts can be stored and reviewed but the run endpoint refuses
  them until PR4.

## The storm model

`internal/control/rmodel/model.go` must follow every migration: CI runs
`stormgen verify -dsn` (drift) and `verify -stale` (rgen). After a migration,
`go run ./cmd/stormgen import -dsn ...` prints the exact model text, then
`go run ./cmd/stormgen generate internal/control/rgen -raw-schema live -dsn ...`.

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
