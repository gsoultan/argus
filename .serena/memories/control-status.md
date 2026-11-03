# Status: argus-control's own health

Settings › Control plane › Status (`web/src/components/ControlStatus.tsx`),
served by `GET /api/v1/platform/status` (`internal/control/control_status.go`).

## Who sees it

`platformOwner` (owner or admin of the default organization); anyone else gets
404 like the other platform routes. Everything on it is platform-wide: the
host, the one database every organization's schema lives in, and every enabled
organization's agents, sessions and recordings, summed. No route reads inside
an organization for it beyond those counts.

## Where each number comes from

- Machine: `/proc` (loadavg, stat, meminfo) and statfs, Linux only
  (`control_machine_linux.go`); other systems report empty fields and the page
  says so. A small reader of its own: the control plane links no agent code.
  CPU busy is measured between two looks (`cpuSampler`); the first look samples
  twice 250 ms apart. Disks are the temp dir (where shipped recordings spool)
  and the recordings directory when one is used, folded by statfs fsid.
- Database: `SELECT 1` timed with a 3 s deadline, every pool's stats summed.
- Slow scans (database size, tables summed by name across schemas, agents
  connected per `agent_credentials.tunnel_seen_at`, sessions, recordings as
  `sum(recording_bytes)` of sessions with a key): cached a minute
  (`statusCache`); a failed refresh keeps the old numbers and their time.
- Recent errors: `a.fail` records op + message in `errorLog` (50 kinds, folded
  by op+message, per-minute ring for the hour). In memory; a restart clears it.
  Never the request or its body.
- Recording uploads: `uploadLog`, set where a shipped recording is stored.
- Certificate: `Settings().TLS.Certificate`.

## Problems

Decided on the server (`statusProblems`), so the pill, tiles and checks agree.
Thresholds are constants at the top of control_status.go: database ≥250 ms or
≥90% of connections; disk ≥80% warning, ≥90% critical; memory ≥90%; an
uploaded certificate under 14 days (a managed one under 7, since it renews
itself); the last recording upload failed; ≥10 failed requests in an hour.

Not on it yet: audit chain verification (no stored result to show; the Audit
page verifies in the browser) and alert channel delivery.
