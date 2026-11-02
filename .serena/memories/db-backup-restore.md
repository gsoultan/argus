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

## Tests

`internal/agent/backup_test.go` needs `ARGUS_TEST_PG_SUPER_DSN` (a local
cluster with pg_dump/pg_restore on PATH); `internal/control/backups_test.go`
needs `ARGUS_TEST_DATABASE_URL`; `cmd/argus-verify/backup_test.go` and
`internal/tunnel/dumpfile_test.go` need nothing. Console screenshots were
taken with a Playwright script that mocks `/api/` and `/auth/me` against a
`VITE_CONTROL_URL=/` build served by `vite preview`; `chromium.launch` needs
a proxy bypass for 127.0.0.1 in the cloud container.
