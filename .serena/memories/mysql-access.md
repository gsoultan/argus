# MySQL and MariaDB access (2026-11)

One engine, `mysql`, for both. Entry = one database (schema) on one server.
Code: `internal/tunnel/mysql.go` (names, quoting, grants),
`internal/agent/mysql.go` (sessions), `internal/control/databases.go`
(`validateMySQL`, `Database.SQL()`), migration 052.

- **Same frames as Postgres.** PGRequest/PGAnswer, FramePGRows batches and
  PGRecord `.argussql` lines, so the console's SQL workspace, recording,
  verification, watch live and replay need nothing new. `ops` the agent does not
  know (`check`, `admin`) answer "unknown operation"; the console treats that as
  an older agent.
- **Per-person users.** `MySQLUser(role, level)` = `argus-<role>-<level>`,
  hashed down to 32 chars. Kept and LOCKED between sessions (dropping would
  break DEFINER views). Open: CREATE USER IF NOT EXISTS, REVOKE ALL, the level's
  GRANTs on that database only (`_`/`%` escaped), ALTER USER one-time password
  ACCOUNT UNLOCK. Last close locks with a random password and KILLs its
  connections; a sweep locks `argus-` users left unlocked. Never touch a user
  not named `argus-`. dba = ALL on the database WITH GRANT OPTION + PROCESS.
- **Admin sign-in.** root over the unix socket (auth_socket/unix_socket), or the
  entry's option file (`[client]`, mode 0600, absolute `.cnf`), or
  `/etc/mysql/debian.cnf`. Every socket is checked against the entry's port via
  `@@port`. MySQL 5.7+ / MariaDB 10.4+.
- **Statements.** `$N` → `?` with InterpolateParams. Cancel and timeout = KILL
  QUERY from a second connection (transaction survives). ROW_COUNT() for
  affected rows. Approved writes are held to their approved row count: own
  transaction, or SAVEPOINT argus_cap inside the person's; "in a transaction?"
  is probed with SAVEPOINT/ROLLBACK TO (error 1305 = no). MySQL 8 has no
  `@@in_transaction`.
- **Console.** `isSQLEngine()` in `web/src/lib/engines.ts` for anything SQL
  (approvals, limits, recording). The workspace hides Postgres-only parts
  (explain, builder, generate, change tracker) via `MySQLDialect` context.
- **Testing.** Agent tests need `ARGUS_TEST_MYSQL_SOCKET` (+ `_PORT`); they
  passed against MariaDB 10.11 and MySQL 8.0.

## Performance analysis (2026-11)

- **Manage › Performance** for mysql (DBA only, badge in the nav). One
  read-only admin op `mysql.perf` (`tunnel.MySQLPerf`), allowed for the mysql
  engine in `internal/control/postgres.go`, refused below DBA by the control
  plane and the agent. Not recorded (`Mutates()` false).
- **Why admin.** The person's `argus-` user has one schema only and no
  performance_schema or PROCESS view of other schemas, so the agent reads as
  the admin sign-in (`adminConn`). `internal/agent/mysql_perf.go`:
  `events_statements_summary_by_digest` (top 25 by SUM_TIMER_WAIT for the
  schema, picosecond timers, with share), global status/variables
  (performance_schema, else information_schema; fixed key lists),
  information_schema TABLES/STATISTICS/INNODB_TRX (open > 5 s)/PROCESSLIST
  grouped, `table_io_waits_summary_by_index_usage`. A source that fails goes
  to `Missing`, the rest still answers.
- **Analysis.** `web/src/lib/mysqlPerformance.ts` (pure, shares Finding and
  formatters with pgPerformance). One-off statements (calls < 5) are never
  heavy; unused indexes only after 7 days of counting; PRIMARY and unique
  indexes are never proposed for dropping. Index fixes use
  `ALGORITHM=INPLACE, LOCK=NONE`. Panel: `DatabaseMySQLPerformance.tsx`,
  reusing `FindingsList`/`Stat` from DatabasePerformance.
- **Testing.** `mysqlPerformance.fixture.json` is a real MariaDB 10.11 answer;
  `TestMySQLPerfOnARealServer` needs `ARGUS_TEST_MYSQL_SOCKET`.
