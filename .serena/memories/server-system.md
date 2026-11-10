# A server's System card

The Overview tab's System card shows what a machine is (OS, kernel,
architecture, processor and CPU count, memory, physical/virtual/container,
boot time, time zone) and how busy it is (processor busy % and load, memory,
swap, each real disk). Code: `internal/agent/sysinfo*.go`,
`internal/control/system.go`, `web/src/components/server/SystemCard.tsx`.

- **Two cadences.** `system` rides the host facts report (checked every 10
  minutes, sent on change or hourly). `usage` rides about one heartbeat a
  minute (`usageSent` in `cmd/argus-agent/main.go`); the other beats send
  none and the row keeps what it holds. Still one write per beat.
- **usage_at is the agent's sample time**, capped at now, and an older sample
  never replaces a newer one: a heartbeat replayed from the spool after an
  outage must not show hour-old figures as current.
- **The console never shows a sample as the present.** The header says when
  it was reported; past 3 minutes, or with the agent not healthy, the figures
  fade under an amber line saying when they are from. Measured against
  `now()` from primitives, so fixture mode uses the fixture clock.
- **Only a filling disk is coloured**: amber from 90%, rose from 95%, counted
  like df (used over used + available, so the root reserve is excluded).
  Other bars are slate: azure is for controls (see design-system).
- **Machine is "" when unknown**, never assumed physical. Physical needs DMI
  to name a vendor and the CPU to carry no hypervisor flag.
- Read-only, nothing a local user could not see: /proc, DMI names under /sys,
  the mount table, statfs. Disks skip virtual filesystems and container/snap
  trees, are one per device, bounded at 32.
- Fleet roles only (`GET /api/v1/assets/{id}/system`), like What runs here;
  anyone else gets no card. Linux only for now; other agents report
  architecture, CPU count and time zone.
- NULL columns (migration 040) mean "not reported": the card then asks for
  an agent update, or says there is no agent.
- **Network** (2026-11): `system.interfaces` is each interface that is up,
  its MAC and its addresses in CIDR form (`internal/agent/netinfo.go`, Go's
  interface table, so Linux and Windows alike). Loopback and link-local are
  dropped. The control plane parses each address (bad ones dropped) and marks
  it public when globally routable: not RFC 1918/ULA, not 100.64/10, not
  reserved. An agent without interfaces falls back to the flat `addresses`
  column, shown unnamed. When no interface holds a public address the card
  says so and why (NAT / cloud public IPs are not on the machine); the agent
  never asks an outside service for its public IP.
- **Windows reports usage too** (2026-11, `internal/agent/sysinfo_windows.go`):
  RtlGetVersion, the registry's processor name, GlobalMemoryStatusEx,
  GetSystemTimes deltas, fixed drives only (GetDriveType), GetIfTable2Ex. No
  load average (`load` is omitted, the console says so instead of 0.00) and
  no swap (the page file is not swap). macOS still reports only the basics.
- **Traffic** rides usage as `net`: per interface that is up and not
  loopback, bytes/s in and out since the previous sample and the stated link
  speed. A counter that went backwards gives no rate (null), never a huge
  one. Linux reads /proc/net/dev and sysfs; Windows drops filter-layer rows.
- **What is "low" is decided once**, `LowResources` in
  `internal/control/resources.go`: disk ≥90% low / ≥95% critical (df-style),
  memory ≤10% / ≤5% left, swap ≥90% used, CPU ≥90% busy. The card gets it as
  `warnings`, the assets list from `GET /api/v1/resources-low` (fleet roles,
  usage under 5 minutes old only), the header pill from the card's query.
  The bell (`resources` kind, migration 074) raises disk and memory only,
  once per asset, resource, level and day, at most 25 per sweep, every 5 min.
  Design: `/mnt/project-files/plans/resource-metrics/`.
- `UsageSampler.Sample` and the system facts recover from a panic in a
  platform reader: a figure is never worth the heartbeat.
