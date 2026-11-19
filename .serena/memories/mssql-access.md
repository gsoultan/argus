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
