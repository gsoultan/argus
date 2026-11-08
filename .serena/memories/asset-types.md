# Asset types

The nav item is **Assets** (route `/assets`, unchanged). Every asset is one
of three types. Design and mockups:
`/mnt/project-files/plans/assets-and-rdp/` (design.md, mockups/).

| Type | `assets.kind` | What it is |
| :--- | :--- | :--- |
| Server | `server` | Nobody sits at it. |
| Workstation | `workstation` | A laptop or PC someone uses. Stored and opened exactly as a server is (ssh or rdp, its own accounts). |
| Device | `android` | A phone or tablet through the Argus app; see android-devices. |

- Migration 063 widens `assets_kind_check`. `AssetInput.normalise` treats
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
