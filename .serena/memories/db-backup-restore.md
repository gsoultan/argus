# Database backup and restore

Postgres databases Argus knows (`databases` rows) can be backed up and
restored from the console. Design and mockups: the project's
`plans/db-backup-restore/`. Code: `internal/tunnel/backup.go` (service,
frames, grant payload) + `dumpfile.go` (the encrypted `.argusdump` format),
`internal/agent/backup.go` + `backup_helper.go` (service, `pg-backup`
helper run as the admin account), `internal/control/backups.go` (tables,
store) + `backup_jobs.go` (runner, retention, sweep) + `backup_routes.go`,
`web/src/components/DatabaseBackups.tsx`, `cmd/argus-verify` (opens a
backup without Argus).

## Shape

- The agent on the database's server runs `pg_dump -Fc` as the local admin
  account over peer auth (`runAs`, like `pg-admin`), through the
  `argus-agent pg-backup` helper, on a `Grant` with `GrantBackup` the control
  plane signed for exactly one job (`ForService` = backup; a dump grant
  carries only the public key, a restore grant only the one data key).
- `.argusdump`: magic `ARGUSDUMP1\n`, a JSON header (backup id, database,
  org, versions, wrapped key), then 1 MiB XChaCha20-Poly1305 frames with the
  header as associated data and a last-frame flag, so nothing can be
  truncated, reordered or moved to another file. Per-backup random data key,
  wrapped to the organization's X25519 key derived
  `HKDF(hex(GrantKey.Seed()), "argus backup v1:"+org)`; nothing stored, it
  rotates with the signing secret. The agent reads the archive back with
  `pg_restore --list` before reporting done (`verified`).
- Destinations: `server` (agent state dir `backups/<db>/<id>.argusdump`,
  root only), `control` (streamed up in 'd' frames, spooled, then
  `Scoped.PutKey` under `backups/<db>/<id>.argusdump` in the recordings
  store), or `both`. The control plane checks the SHA-256 the agent reported.
- Tables (migration 032): `database_backup_schedules`,
  `database_backups` (one running per database, unique partial index),
  `database_restores`; `access_requests.restore_id`. All per organization
  through `Store.db(ctx)`.
- `API.BackupSweep` runs in the 30 s sweep per tenant: due schedules (missed
  if the agent is offline, run late if under 6 h, alert audit after 3
  misses), stale running rows (10 min without a report → failed), retention
  (`keep_last` + `keep_days`, pinned never, a backup under restore never),
  and dropping the copies a replace kept after 24 h.
- Restore: `new` (Owner+, into a new name, the new database is added to
  Argus with nobody assigned) runs at once; `replace` (DBA, typed name,
  reason) creates an `access_requests` row with principal `db:restore`
  linked to the restore, and the decision route runs it on approval
  (`restoreApproved`) with the request id as the grant's `ApprovalID`, which
  the agent refuses without. The agent renames the current database to
  `<name>_before_restore_<ts>` (`tunnel.KeptMarker`), `ALLOW_CONNECTIONS
  false`, restores into a fresh database, rolls back on failure; "Put it
  back" (undo) swaps the kept copy in again.
- Audit: `db.backup_*`, `db.restore_*` through `auditDB`. Downloads of the
  encrypted file (DBA, only when a control-plane copy exists) are audited.

## Test restores

Plans: `plans/db-backup-test-restore` in the project files. A test restores
a backup into `argus_test_<8 hex>` (`tunnel.ValidTestDB`; nothing else may
use that prefix: databases.go and restore-into-new refuse it, discovery
hides it) on the database's own server, checks it, and always drops it.

- Agent (`internal/agent/backup_testrestore.go`, `postgres_testcopy.go`):
  jobs `test`, `checks`, `drop-test` on the backup service. Room check
  (source × 1.3 + 1 GB against `data_directory`), hash check, TOC TABLE DATA
  list, `CREATE DATABASE … TEMPLATE template0 CONNECTION LIMIT 0`,
  `pg_restore --no-owner` into it under nice/ionice (`Existing`, `Low` on
  `PGBackupRequest`), exact counts (60 s per table, -1 = not counted), index
  validity, size, custom checks each in a READ ONLY transaction, drop.
  `serverJobs`: a test runs alone on its server; dumps/restores take a slot.
- Control plane judges (`evaluateTest` in `backup_testrestore.go`) against
  the last passing test, not the live database: missing table or invalid
  index fails; ignored restore errors, an emptied (>1000 rows before) or
  halved table, uncounted tables, or a copy under half the size warn;
  `argus_trash.` is skipped; custom checks fail on false/error; no room is
  "skipped". Rows: `database_backup_tests` (migration 045).
- Schedule: `test_every` off/after/daily/weekly (+hour/minute/weekday,
  `test_checks` jsonb). "after" is refused with hourly backups. The sweep
  (`backupTestSweep`) fails stale tests, retries dropping leftover copies,
  runs due tests (waits up to 2 h when busy/offline, then skipped) and
  raises "no passing test" once per stretch after 2 × interval + 1 day.
- A backup preempts a running test (`makeWayForBackup`): recorded as
  skipped, retried an hour later, so a pre-drop backup never waits.
- Retention never deletes the newest backup that passed, or one under test.
- Alerts: kind `backup` (`alertBackup`), also sent through
  `Alerts.SecurityEvent`. Scheduled-run failures count with misses toward
  the 3-in-a-row alert (`CountScheduledOutcome`).
- Routes: `POST backups/{b}/test` and `POST backup-tests/{t}/cancel` (DBA),
  `GET backup-tests/{t}` (full report with rows by table; the list strips
  them), `POST backup-checks` (admin, read-only dry run on the live
  database, audited). `GET backups` adds `tests`, `testing`, `lastGood`.
- Console: last-known-good card, Tested column, report modal, Settings
  "Test restores" with a checks editor. Test:
  `web/src/components/__tests__/backupTests.test.tsx`.
- Custom checks are read by the query builder's shared classifier
  (`tunnel.ClassifySQL`, wrapped by `tunnel.ReadOnlyCheckSQL`).
- Not yet: marking row changes made through the query builder's Data
  changes log as expected.

## Tests

`internal/agent/backup_test.go` needs `ARGUS_TEST_PG_SUPER_DSN` (a local
cluster with pg_dump/pg_restore on PATH); `internal/control/backups_test.go`
needs `ARGUS_TEST_DATABASE_URL`; `cmd/argus-verify/backup_test.go` and
`internal/tunnel/dumpfile_test.go` need nothing. Point the agent tests'
super DSN at a scratch database (e.g. `/agenttest`), not `argus`, or their
probe table shows up as storm drift. Console screenshots were
taken with a Playwright script that mocks `/api/` and `/auth/me` against a
`VITE_CONTROL_URL=/` build served by `vite preview`; `chromium.launch` needs
a proxy bypass for 127.0.0.1 in the cloud container.
