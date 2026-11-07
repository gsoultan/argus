# Installing and removing software

Install and remove packages on a Linux server from the Installed segment of
What runs here (Apps tab). Design: /mnt/project-files/plans/app-install/
(design.md, mockups). Shipped in order: PR1 Linux (this), then the software
library with scripts, Android, Windows, many servers at once.

Agent: `internal/agent/software*.go`. Shared: `internal/tunnel/software.go`
(grant, validation, result types). Control: `internal/control/software_routes.go`
plus install/remove fields on package runs in `package_updates.go`. Migration
057. Console: `web/src/components/server/InstalledSoftware.tsx`,
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
- Tables `software_scripts`, `software_script_versions` (migration 058).
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

## Part 3: apps on Android devices (migration 059)

- `internal/apk` (shared package, in the layering list) reads an APK: the
  binary manifest (package, version code/name, label, min SDK) and the
  signer (SHA-256 of the first cert from the v3.1/v3/v2 signing block, else
  the v1 PKCS7). `apk/apktest` builds signed test APKs.
- Library files: table `software_files` (android|windows, apk|msi|exe),
  object at `software/<sha>.apk` in the backup store. Routes in
  `internal/control/software_files.go`: GET/POST/DELETE
  /api/v1/software/files, POST /api/v1/assets/{id}/android-apps/runs
  {action install|uninstall|disable|restore}, and the device fetches
  /api/v1/report/software-file/{run} (only its own queued/running install).
- Runs are package_runs with modes app_install/app_uninstall/app_disable/
  app_restore and `software_file_id`. protectedApps (Argus, Settings, System
  UI, Play, GMS, installer, phone, android) are refused on both ends.
  Disable/restore only with the unattended engine; preinstalled apps are
  never uninstalled.
- Device: `internal/agent/android_install.go` (AppRuns) downloads, checks
  size, SHA-256, package, version code and signer, then calls the Kotlin
  `AppInstaller` (PackageInstaller session when attended, so the person can
  say no; `pm` via the helper when unattended).
- Console: Settings, Software, "Apps for devices" card; the device Apps tab
  has Install app…, a per-row menu, a waiting banner and Recent changes.
- Stacked on PR #182 (Android Apps tab, 056_android_apps.sql) by a merge;
  this branch's migrations are 057, 058, 059.

## Part 4: Windows servers (migration 060)

- Agent (`internal/agent/winsoft.go`, no build tag, tested anywhere;
  `winsoft_windows.go` reads HKLM Uninstall in both views and launches the
  helper detached with a pid file). Programs get ids `msi-<guid>` or
  `app-<hash>` (valid package names) and a `label` (display name); labels
  ride SoftwareItem/SoftwareChange/InstalledPackage. Removal runs only
  `msiexec /x {GUID} /qn /norestart` or the program's own
  QuietUninstallString (never cmd/PowerShell); no quiet uninstaller means
  protected. Protected: Argus, Edge/WebView2, .NET, VC++ redistributables,
  "Windows ...", OpenSSH.
- PackageBeat.Installs (agent and control): modes a non-updatable host still
  runs; Windows says remove, script, file_install. softwareReady accepts a
  host with Installs. scriptPlatformFor picks windows scripts for them.
- Mode `file_install`: library MSI/EXE (software_files platform windows,
  meta FileWindows{args, signer, subject}). EXE needs silent switches; EXE
  Authenticode signer read via debug/pe + apk.PKCS7Certificates; MSI
  signature not read (warned). The daemon downloads before launching (the
  helper never talks to the control plane). Production needs a done run
  outside production first. Route POST /assets/{id}/software/file-runs.
- PowerShell scripts: powershell.exe -NoProfile -NonInteractive
  -ExecutionPolicy Bypass -File, prelude `$ErrorActionPreference='Stop'`,
  UTF-8 BOM, params in env, runs as LocalSystem (run-as refused).
- Console: a Windows (rdp) server's What runs here is the Installed list;
  the Install drawer offers Library file | PowerShell script.

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
