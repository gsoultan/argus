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
