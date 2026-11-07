# Log cleanup

The agent reports what logs take on its server's disks and removes them when
the console asks or a tag's rule says so. Agent: `internal/agent/logclean*.go`.
Control plane: `internal/control/log_cleanup.go` (store, planning, previews)
and `log_cleanup_routes.go` (heartbeat part, routes, audit, alerts). Migration
055. Console: the server's Logs tab (`web/src/components/server/LogsTab.tsx`),
Settings › Log cleanup (`LogCleanupSettings.tsx`), queries in
`web/src/lib/logQueries.ts`, a "Free space…" link on a filling disk in the
System card. Live only: nothing here has a fixture.

## How a cleanup travels

- Same shape as package updates: everything rides the heartbeat (`logs` both
  ways). A run is on every reply until the agent reports how it ended;
  `sent_at` set means it cannot be taken back. No news for an hour: the
  control plane calls it interrupted (`step = 'stale'`).
- Linux: the scan and the cleanup run as root through `systemd-run` (scan
  with `--pipe --wait`, cleanup in a unit `argus-logclean-<id>`), because the
  agent's own service is `ProtectSystem=strict` and cannot write /var/log.
  Windows: in process (the agent is SYSTEM), the cleanup in a goroutine.
- The scan reads names, sizes and dates only, never contents. Items: journal,
  rotated, live logs over 1 GB (or the rule's threshold), Docker json logs,
  rule paths; Windows: IIS, Windows\Logs, Windows\Temp (7 days), dumps,
  event logs (read only).
- A cleanup is planned only from the scan's own file list
  (`planLogCleanup`); the console cannot send a path. The helper checks each
  file again: plain path, under the item's root or the rule glob, not
  protected, a regular file not through a symlinked folder, same size within
  10%, not modified since the scan, older than the keep days. Anything else is
  skipped and listed. The manifest (path, size, date, action) is kept with the
  run; the contents are not.
- Manual cleanups need the server's short name typed (`confirm`); a rule's
  `needs_approval` makes them wait for a second admin, not the asker.

## Evidence that is never cleaned

Login records (auth.log, secure, wtmp, btmp, lastlog, faillog, tallylog,
sudo*), /var/log/audit/, Argus's own files, the Windows Security log,
ProgramData\Argus, winevt. Event logs are never cleared: the only action is
raising a maximum (to at most 4 GB). The journal always keeps at least 7 days
and 256 MB. Settings can add protected paths, never remove these.

## Rules

- Per tag, first match wins, plus the '' rule (seeded off). Modes off, disk
  (at most one automatic run per server per day when a disk passes the
  percent), daily (a window by the server's clock). Rule globs must be
  absolute, contain "log", and stay out of system and home folders
  (`ValidLogGlob` / `validLogGlob`, kept in step).
- Automatic runs never take dumps, and take live or container logs only when
  `active_over_mb` is set. Live logs are truncated keeping the tail
  (`active_keep_mb`, default 100), never deleted.

## Things that bit

- storm's column builder has no date type: `auto_day` is TEXT `YYYY-MM-DD`.
- `humanBytes` already exists in backup.go.
- Background processes started from a sandboxed shell die with it; a live
  test holding a deleted file must run in the same command.
