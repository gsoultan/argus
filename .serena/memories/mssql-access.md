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
- **Testing.** Agent tests need `ARGUS_TEST_MSSQL_SA_PASSWORD` (+ `_PORT`).
- **Plan.** PR 1 foundation; then logins/Grant-Revoke-Deny, DBA tools
  (activity, blocking, waits, index/stats, DBCC, Agent jobs), backup/restore/
  create database, replication (AGs, transactional, log shipping).
