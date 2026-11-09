# Asset types

The nav item is **Assets** (route `/assets`, unchanged). Every asset is one
of three types. Design and mockups:
`/mnt/project-files/plans/assets-and-rdp/` (design.md, mockups/).

| Type | `assets.kind` | What it is |
| :--- | :--- | :--- |
| Server | `server` | Nobody sits at it. |
| Workstation | `workstation` | A laptop or PC someone uses. Stored and opened exactly as a server is (ssh or rdp, its own accounts). |
| Device | `android` | A phone or tablet through the Argus app; see android-devices. |

- Migration 065 widens `assets_kind_check`. `AssetInput.normalise` treats
  `workstation` like `server` (no control mode, no approval flag).
  `Store.EnrolHostAs(kind, …)` enrols a discovered host as server or
  workstation; `EnrolHost` is `EnrolHostAs("server", …)`. A device row keeps
  its kind if a host of the same name is enrolled through the host path.
- Console: `lib/assetType.ts` maps kind to type and labels. Tabs per type are
  `TABS_BY_TYPE` in `components/server/tabs.ts`, same order everywhere, a tab
  that does not apply is left out and a link to it lands on Overview:
  server = Overview, Access, Sessions, Apps, Packages, Logs, Agent, Settings;
  workstation drops Logs and calls Packages "Updates"; device drops
  Updates and Logs and calls Agent "App on device". The agent card moved out
  of Settings into `AgentTab`.
- Not built yet (later PRs): session list before Connect and signing out idle
  Windows sessions on servers; asking the signed-in person on a Windows
  workstation and telling them on a Linux one; per-type policies; fleet
  update notices. Today nothing asks on Windows; any prompt comes from
  Windows' own session rules.

## Before connecting (steps 2 and 3)
- `logons` tunnel service (agent/logons*.go, tunnel/logons.go): who is signed in (WTS on Windows, utmp on Linux) and the Remote Desktop limit (1 desktop edition, 2 server without Session Host). Shown on the Connect page (components/connect/SignedIn.tsx). Admins may sign out a disconnected Windows session; the agent refuses active, console or changed-hands sessions. Audited as asset.session_signed_out / asset.session_sign_out_refused.
- Workstation grants carry `tunnel.GrantAsk` (agent/ask*.go): desktop and screen ask the active session first via WTSSendMessageW (60 s, no answer = no); terminals only tell (notify-send on Linux, a message box on Windows). Nobody using it means nobody is asked. A refusal starts with tunnel.AskRefusedPrefix and is audited as session.declined_at_machine. Servers carry no Ask.
- Step 4: rules per type in gateway_policy.connect_rules (migration 066; '{}' = built-in defaults in control/connect_rules.go: server never/never, workstation ask/tell, 60 s) and assets.connect_ask (NULL follows type; never|ask|tell; refused on devices). `API.askFor` decides every desktop/screen/terminal grant's Ask. Settings › Asset types (components/AssetTypeSettings.tsx), and "Before connecting" on the asset edit form. A terminal is never asked, only told.
- Step 5: update notices. `GET /api/v1/updates-waiting` (control/updates_waiting.go) joins PackageFleet and the agent fleet. `RunUpdateDigest` (hourly, ForEachTenant) raises one "updates" bell alert per kind per day (subject updates:<security|agents|restart>:<date>; migration 067 widens console_alerts_kind_check). Console: Assets › Updates page (routes/assets.updates.tsx), bell Updates tab, Needs updates filter and badges on the asset cards (lib/updatesWaiting.ts). Overview weight budget raised to 309 for it.

## macOS workstations (design: /mnt/project-files/plans/mac-and-iphone/)
- Logons on darwin read /var/run/utmpx (agent/utmpx.go; 628-byte records with a 32-bit timeval, 640 accepted too, any other layout is an error, never "nobody") plus the owner of /dev/console. The screen is line "console".
- Ask and tell on darwin (agent/ask_darwin.go): osascript dialog or notification in the person's session via `launchctl asuser <uid> sudo -n -u <user>`; text passed as argv, never as script. Before this a Mac read Linux's utmp, found nobody and opened without asking or telling.
- Installer: packaging/macos/build-pkg.sh (pkgbuild, id com.argus.agent; productsign when DEVELOPER_ID_INSTALLER is set). preinstall boots the job out, postinstall starts it only when /var/db/argus/enrollment.json exists. release.yml `macos-installer` builds one per arch from the darwin tar.gz, cosign-signs it; Developer ID from MACOS_INSTALLER_P12 / _PASSWORD / _IDENTITY secrets, optional. Uninstall forgets the receipt. CI's macos-agent drill installs via the .pkg, upgrades over itself, uninstalls.
- Not built yet: remote desktop over macOS Screen Sharing (RFB on 127.0.0.1:5900), Apps/Updates, console agent update. iPhone: view + inventory app proposed, no remote control possible on iOS.
