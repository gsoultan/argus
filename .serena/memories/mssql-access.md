# SQL Server access (2026-11)

Engine `mssql`. Entry = one database on one SQL Server instance (2016 / 13.x+),
Linux hosts only for now. Code: `internal/tunnel/mssql.go` (names, quoting,
roles, `MSSQLWrites`), `internal/agent/mssql.go` (sessions, go-mssqldb),
`internal/control/databases.go` (`validateMSSQL`), migration 098.

- **Same frames as Postgres**, like MySQL: PGRequest/PGAnswer, FramePGRows,
  PGRecord. The console workspace uses `SQLDialect` context
  (`'postgres'|'mysql'|'mssql'`); `notPg` hides Postgres-only parts. SQL Server
  shows the Query tab only, opens `dbo`, quotes `[..]`, formats as transactsql.
- **Per-person logins.** `MSSQLLogin(role, level, db)` =
  `argus-<role>-<level>-<8hex sha256(db)>` (a login is server-wide, so one per
  database). Kept and DISABLED with an unknown password between sessions
  (never dropped: a user may own objects). Open: strip every role membership
  and class 0/1/3 permission, then grant the level's (read db_datareader;
  write +db_datawriter +EXECUTE; owner +db_ddladmin; dba db_owner + server
  VIEW SERVER STATE; all get VIEW DEFINITION, SHOWPLAN). Never sysadmin. Last
  close disables and KILLs the login's sessions; a sweep disables stale
  `argus-%` logins. Database-side DDL runs on one held `sql.Conn` because the
  driver's pool reset undoes `USE`.
- **Admin sign-in.** `user =` / `password =` in a file on the server,
  `/etc/argus/mssql.conf` by default (absolute, `.conf`, mode 0600). Needs
  ALTER ANY LOGIN and ALTER ANY CONNECTION. Loopback only, encrypt=false.
- **Statements.** `$N` → `@pN`. Cancel/timeout = per-statement context (driver
  sends attention; transaction survives). Messages via sqlexp ReturnMessage:
  rows affected summed, PRINT sent as notices, first result set shown. Row cap:
  `@@TRANCOUNT`, own BEGIN/COMMIT or `SAVE TRANSACTION argus_cap`.
- **Approvals.** A batch needs no semicolons ("SELECT 1 DELETE FROM t"), so
  `statementGate` uses `tunnel.MSSQLWrites`: any reserved write word anywhere,
  or a first word that is not a read. The guard still reads the first statement.
- **Logins & access (PR 2).** `internal/tunnel/mssqlaccess.go` builds the
  `ms.*` admin ops (users with a new SQL login / existing login / none,
  roles, ALTER ROLE ADD/DROP MEMBER, GRANT/REVOKE/DENY on DATABASE::,
  SCHEMA::, OBJECT::); `internal/agent/mssql_access.go` runs them as the admin
  login in one transaction, recorded first. Passwords become SQL Server's
  hash in the agent (0x0200 + salt + SHA-512, `PASSWORD = 0x.. HASHED`); the
  text says `[redacted]`. Refused: `argus-*`, dbo/guest/sys/public, changing
  fixed roles; the agent refuses touching a login that is not the user's,
  holds a server role or CONTROL SERVER, or (to drop) has users elsewhere.
  `mssqlAccessGate` in control: waits where changes wait; db_owner
  membership and CONTROL/TAKE OWNERSHIP ON DATABASE always wait. Effective
  permissions come from EXECUTE AS USER + fn_my_permissions/HAS_PERMS_BY_NAME.
  Console: `DatabaseMSSQLAccess.tsx` (lazy), Manage tab for mssql shows
  `MSSQL_MANAGE` only.
- **DBA tools (PR 3).** `internal/tunnel/mssqldba.go` builds `ms.*` DBA ops
  (`MSDBA *MSSQLDBASpec`): reads ms.activity / performance / indexes /
  integrity / jobs / settings; changes ms.kill, index.rebuild/reorganize,
  stats.update, checkdb, job.start/stop/enable, db.set (whitelisted
  settings). `MSSQLAdminKind`/`MSSQLAdminText` cover access + DBA; the
  control gate is `mssqlAdminGate`; SET RECOVERY SIMPLE always waits. Agent
  `internal/agent/mssql_dba.go`: one statement, no transaction (ALTER
  DATABASE / DBCC refuse one); upkeep and CHECKDB run up to 25 min as the
  session's current request so Cancel reaches them. Reach: KILL only a user
  session in this database or blocking one here; jobs only with a step whose
  database_name is this one. Console `DatabaseMSSQLDBA.tsx` (lazy panels,
  `ConfirmOp` modal with the agent's preview); all SQL Server Manage panels
  are DBA only. Local test container has SQL Server Agent enabled
  (`mssql-conf set sqlagent.enabled true`).
- **Testing.** Agent tests need `ARGUS_TEST_MSSQL_SA_PASSWORD` (+ `_PORT`).
- **Plan.** PR 1 foundation; then logins/Grant-Revoke-Deny, DBA tools
  (activity, blocking, waits, index/stats, DBCC, Agent jobs), backup/restore/
  create database, replication (AGs, transactional, log shipping).
- Create database (PR 4a): dbcreate service takes `mssql:{collation,recovery}`; plan is `CREATE DATABASE [n] COLLATE c` then optional `ALTER DATABASE [n] SET RECOVERY r`. Agent `createMSSQL` checks name free and collation in sys.fn_helpcollations(); options list collations + model's recovery. Web: CreateOnServer mssql fields; the "Create new" toggle is now shown for every engine.
- Backup/restore (PR 4b): GrantBackup gained Engine+ConfigPath; mssql jobs = dump, restore, catalog, prune, drop, undo (no test/checks). Agent `backup_mssql.go`: BACKUP DATABASE ... COPY_ONLY, CHECKSUM (+COMPRESSION unless Express) to a staging folder (admin file `backup_dir` / `backup_dir_local`, default InstanceDefaultBackupPath), RESTORE VERIFYONLY, encrypt to .argusdump, read back, delete .bak. Restore decrypts to staging (chown like folder), FILELISTONLY, WITH MOVE to `<target>_<id8>_<fileid>.mdf/ndf/ldf`. Replace renames via SINGLE_USER + MODIFY NAME. Control: `backupEngines` = postgres+mssql; test-restore routes `withPostgres`; schedule forces TestEvery off for mssql. Live test needs container volume: ARGUS_TEST_MSSQL_BACKUP_DIR=/tmp/argus-msbak:/var/opt/mssql/argus-backup (dir chowned 10001).
- Replication (PR 5): `tunnel/mssqlrepl.go` kinds `ms.replication` (read), `ms.ag.add/remove/suspend/resume/failover`, `ms.repl.distributor/publish/pub.add/pub.drop/article.add/article.drop/sub.add/sub.drop/snapshot`, `ms.ls.backup/copy/restore`; spec on `PGAdminOp.MSRepl`. Texts keep placeholders `{admin login}`, `{admin password}`, `{subscriber password}`; agent `mssql_repl.go` fills them only at run time, so recordings hold no password. AG DDL runs from `USE master`. Dropping the last publication / publish off stops the Log Reader Agent job first (error 18752), with one retry. Always approval: distributor, ag.remove, ag.failover, pub.drop, publish off. Log shipping is monitor + run its jobs only (set up in SSMS). Console `DatabaseMSSQLRepl.tsx` (Manage > Replication, DBA). Live test needs the container started with `MSSQL_ENABLE_HADR=1`, `MSSQL_AGENT_ENABLED=true` and a `--hostname`; AG tested with one replica, CLUSTER_TYPE NONE.

## Performance analysis (2026-11)

- **Manage › Performance** (was "Waits & queries"), DBA only like every SQL
  Server panel. It reads four fixed read-only ops at once: `ms.performance`
  (CPU sort), `ms.indexes` (now with each index's key columns, `keys`),
  `ms.settings`, and the new `ms.health` (`internal/agent/mssql_health.go`):
  version/edition, uptime, CPUs, memory, process memory, performance counters
  (page life expectancy, memory grants pending, batches, compilations,
  recompiles, deadlocks, page splits), tempdb data files and sizes, VLFs
  (`sys.dm_db_log_info`, 2016 SP2+), plan cache and single-use ad hoc MB,
  blocked requests and oldest transaction now, Query Store state. A source
  that fails goes to `Missing`.
- **Analysis.** `web/src/lib/mssqlPerformance.ts`, pure, shares Finding with
  pgPerformance. Waits are explained only when ≥10% of waiting, ≥60 s and
  ≥0.5% of uptime. A heavy statement is tied to SQL Server's own missing
  index on the table it reads (then the missing index is not repeated).
  Unused indexes only after 7 days; PK, unique and clustered never dropped.
  CREATE INDEX / REBUILD get `WITH (ONLINE = ON)` on Enterprise/Developer.
  Server options use `sp_configure`; database ones `ALTER DATABASE CURRENT`.
- **Testing.** `mssqlPerformance.fixture.json` is a real SQL Server 2022
  answer. A local SQL Server runs as `dockerd &` then `docker run ...
  mcr.microsoft.com/mssql/server:2022-latest`; `TestMSSQLHealthOnARealServer`
  needs `ARGUS_TEST_MSSQL_SA_PASSWORD`. `TestMSSQLReplication` fails on such
  a plain container on main too ("no recording at dba").
