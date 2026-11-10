# Postgres access

Databases are a fourth thing a person opens from the console, after a
terminal, files and a desktop. Design and mockups: the project's
`plans/postgres-access/`. Code: `internal/tunnel/postgres.go` (frames, grant
payload, row encoding, recording chain), `internal/agent/postgres*.go`
(session, role provisioning helper, recording), `internal/control/databases.go`
+ `database_routes.go` + `postgres.go` (tables, API, ticket, relay),
`web/src/components/DatabaseWorkspace.tsx` and `web/src/routes/databases.*`.

## Shape

- A `databases` row belongs to an asset; the agent on that asset opens it over
  a local socket or loopback only (`tunnel.LocalPGHost`). `database_assignments`
  gives a team or a person a level: read / write / owner / dba, optionally
  "needs approval" (default on for write+ on a production label, decided in
  the console, not the server).
- Levels are Postgres group roles `argus_<level>_<db>` plus `argus_people`;
  each person gets `tunnel.PersonalRole(login)`, marked with a `COMMENT`
  (`argus-managed owner=<email>`) so the helper never touches a role it did
  not make. Enforcement is Postgres grants, never a SQL parser. Owner/DBA
  sessions `SET ROLE` to the level role so what they create belongs to the
  level, not the person.
- The ticket route (`POST /api/v1/databases/{id}/ticket`) decides the level
  (`DatabaseAccess`: highest row that needs no approval; an approval row counts
  only while an approved request is unexpired; admins are dba). The relay
  signs a `Grant` with `GrantDB` (db id, name, host, port, level, limits) and
  the agent verifies it like any grant. Principal is `role@level`.
- The agent runs `argus-agent pg-admin` as the `postgres` account (peer auth,
  `runAs` shared with files) to open/close the role: NOLOGIN until a session,
  one-time SCRAM verifier with `VALID UNTIL`, locked and password cleared on
  close, backends terminated. The daemon never holds a superuser connection.
- Connect order: the database's host, then the other local way in (socket →
  127.0.0.1 or the reverse). A socket dir is usually under `local all all
  peer`, which refuses our roles; loopback is usually scram. Most servers need
  no pg_hba change; `argus-agent install` prints the line if they do.

## Many at once, and the ones added later (2026-11)

`database_bulk.go`: `POST /api/v1/database-access` (and `/plan`, `/remove`)
gives teams and people a level on many databases and brokers in one
transaction. It writes ordinary `database_assignments` rows, one audit entry
per row written, and never lowers anyone unless `lower` is set; the plan and
the write share `planGive`, so the console's preview is what is written.
`later` adds a `database_access_rules` row (migration 069) per who and kind
(database or broker) on that one server; `applyRulesTo` writes rows for it
when a database is added (console or restore-into-new). Rows remember
`rule_id`; a hand edit through the single PUT clears it, so taking a rule
away with `takeAway` takes back only what is still its own. Deciding whether
a session opens never reads the rules table.

## Living beside other applications

- Argus only ever creates and alters roles that carry its `argus-managed`
  comment, and only adds grants to its own level roles. Nothing revokes,
  drops or terminates anything else; `TestPostgresLeavesOtherRolesAlone`
  snapshots an application's roles, ACLs and default privileges around
  sessions at every level. Keep it that way.
- The DBA level role holds read/write/owner `WITH ADMIN OPTION`, which is how
  the console's Roles panel grants a level to an application role; nothing
  below DBA can.
- PgBouncer: `GrantDB.PoolHost/PoolPort` (from `databases.pooler_host/port`)
  make the agent connect through the pooler first, falling back to Postgres.
  A pooled session sends no startup parameters but application_name
  (PgBouncer refuses unknown ones) and runs every statement as
  `BEGIN; SET LOCAL …; SET LOCAL ROLE …; <stmt>; COMMIT` unless the person is
  inside their own transaction, so transaction pooling is safe. PgBouncer
  authenticates Argus roles via `auth_query` against
  `argus_auth.pgbouncer_lookup(text)`, a SECURITY DEFINER function that
  answers only for argus-managed members of `argus_people`;
  `tunnel.PoolerAuthSetupSQL` is the only copy, run by the agent before every
  pooled open and shown by the console's Connection tab. The schema is
  `argus_auth`, not `argus`, because a role named `argus` would otherwise get
  its unqualified tables created there (`$user` in search_path).
- `TestPostgresPooledSession` runs a real PgBouncer in transaction mode when
  `pgbouncer` is on PATH (as `postgres` or `nobody` if root, since PgBouncer
  refuses to run as root).

## Console administration panels

`web/src/components/DatabaseAdmin.tsx`: Activity & locks and Roles & access
are plain recorded statements through `collect()` in DatabaseWorkspace, which
gathers one request's rows by id without touching the editor's result. The
`ready` answer carries `levels` (level role names) and `via`. Non-DBA levels
see a real gate on Roles; Activity works at every level with Postgres hiding
what the level may not see.

## Wire and cost

- Frames: `q` request, `a` answer (JSON), `r` row batch (binary: uint32 id
  then uvarint-prefixed cells, 0 = NULL, low bit = truncated at `MaxCell`),
  `l` recording line. One statement per request (extended protocol), request
  queue of 8, rate limit per minute, `MaxRows` ceiling 100,000.
- `RowEncoder` is zero-allocation per row (benchmark in `postgres_test.go`).
  The browser decodes batches into string arrays and renders a windowed
  table; 50,000 rows measured at ~1 s end to end and ~93 MB heap.
- Cancel is a context cancel plus an explicit `CancelRequest`, so a running
  statement stops within ~0.5 s rather than at `statement_timeout`.

## Recording

`.argussql`: one JSON line per event (open, stmt, done/error/cancel, close),
written before the statement runs, each line hashed over itself with `hash`
empty and naming the previous hash in `prev`. `tunnel.VerifyPGRecording` is
what the control plane's recording route and `argus-verify` use; it reports
the first broken line. The session row is `kind = 'database'`,
`protocol = 'postgres'`, `fidelity = 'statement'`, and the console replays it
as a statement list (`PostgresReplay`).

- **A statement is recorded whole or not run (2026-11).** A recording line
  crosses to the control plane as one frame (`tunnel.MaxFrame`, 256 KB), and
  JSON can make text up to six times longer (`<` is `\u003c`). So the bound
  is `tunnel.RecordedTextFits` on the encoded text, checked by the control
  plane and the agent before anything runs; both recorders also refuse a
  line past `MaxRecordLine` without writing it (`ErrRecordTooLong`), and the
  session goes on. `MaxSQL` and `MaxCommand` are 192 KB. Before this, a
  statement over ~190 KB failed verification (`VerifyPGRecording`'s scanner
  was capped under a frame), and one past a frame left the session deaf.
  The replay shows the first 1,500 characters with "Show all"; the file
  always holds the whole statement.
- **Watch live** on a database session sends the viewer recording lines
  (`{"type":"record","data":<line>}`) from the session's hub, broadcast under
  `recMu` so the order is the stored order; `StatementWatch` renders them
  with `PostgresReplay`. Result rows never go to the hub: they are not
  recorded, and a viewer sees what the recording holds. A late viewer gets
  the hub's 256 KB backlog of recent lines.
- The recording route answers 409 "still running" for an active session
  with no key yet, and the session page polls an active session so the
  replay appears when it ends instead of after a reload.

## Testing

- `internal/agent` Postgres tests need `ARGUS_TEST_PG_SUPER_DSN` (a superuser
  on a scratch database) and skip otherwise; `internal/control` database tests
  need `ARGUS_TEST_DATABASE_URL` as usual.
- The agent test harness talks to the service over `net.Pipe`, so a test must
  keep reading frames while it waits (a cancel sent from the test goroutine
  during `h.run`); a blocked `record()` otherwise looks like a slow cancel.
- Session ids are UUIDs minted by the agent (`sessionUUID`); the sessions
  table is keyed by uuid, and the first end-to-end run found a `pg-<hex>` id
  silently failing every upsert.
- The settings route is `databases.$dbId_.settings.tsx`: the underscore keeps
  it out from under the workspace route, which has no `<Outlet/>`.
- The control plane decodes request bodies with DisallowUnknownFields, so a
  console mutation must not pass its `{id, ...}` variables object as the
  body; strip the id first (this silently 400'd every settings save once).

## Editor autocomplete

`web/src/lib/sqlComplete.ts` (pure, tested) and `web/src/components/Autocomplete.tsx`
(a list under the caret of the native textarea; Ctrl+Space, ↑/↓, Enter/Tab, Esc).
Names come from the `schema` op as the person's role, like the tree, and are
not recorded: `kind: "completion"` answers schemas, the search_path, tables,
views, sequences and functions in one go (privilege-filtered the same way as
the tree, 20,000 rows max); columns come per table from `kind: "columns"`
only once a statement names the table, cached for the session (500 tables
max). An agent too old to know "completion" answers an error, which leaves
keywords and built-in functions. `web/src/lib/redisComplete.ts` is the same
for the Redis command line in `RedisWorkspace` (commands, subcommands, keys
the browser listed); the open list takes Enter and ↑/↓ before run and history.

## Extensions and logical replication (2026-11)

Design: the project's `plans/postgres-extensions-replication/`. Two more
left-bar entries, *Extensions* and *Replication*, in
`web/src/components/DatabaseExtensions.tsx` / `DatabaseReplication.tsx`.

- **Not plain recorded statements.** `CREATE SUBSCRIPTION`, `FOR ALL TABLES`
  and most `CREATE EXTENSION`s need superuser, which the DBA level role is
  not and stays not; and a subscription statement carries the publisher's
  password, which must never reach the recording. So they are a new `admin`
  op on the session (`tunnel.PGAdminOp`, kinds `extension.*`,
  `publication.*`, `subscription.*`) that the agent hands to the pg-admin
  helper (`postgres_admin_ops.go`): a fixed list, identifiers through
  `quoteIdent`, the conninfo built from fields with libpq quoting.
- **Built twice, pure.** `buildAdminSQL` has no database, so the agent
  records the redacted statement *before* the helper runs the real one and
  the helper rebuilds it from the same fields. The real statement exists
  only in the helper process. `Preview: true` returns the redacted text
  without running or recording, which is what the forms show above their
  button; the console never composes this SQL itself.
- **DBA decided twice.** The control plane refuses `admin` below dba (quick
  answer, no logging of the request); the agent refuses unless the *signed
  grant* says dba (the one that counts). Production therefore rides the
  existing DBA approval; there is no new approval type.
- **Recording and audit.** A mutation is an `admin` line (kind `admin`,
  SQL redacted) then `done`/`error`; lists and previews are not recorded.
  The control plane appends `db.admin` (warning) per `admin` line and counts
  it in `CommandCount`. `PostgresReplay` shows the line with "as the
  server's postgres account".
- **The password.** Argus stores none of it. Postgres holds the connection
  string in `pg_subscription.subconninfo` as always (superuser-visible);
  the list goes through `redactConninfo` (both libpq forms) before leaving
  the helper. `TestPostgresAdminOps` asserts the secret is in Postgres and
  nowhere else: not the recording, not the answer, not the list.
- Dropping a subscription whose publisher is gone needs
  `SET (slot_name = NONE)` first or `DROP` fails trying to reach it;
  `PublisherGone` does that and the form says the slot is then theirs.

### Changing what exists (2026-11, second PR)

Lists read `pg_publication` / `pg_subscription` whole, so anything made
outside Argus is found; `SetUpBy` is empty for those (the UI says "not
through Argus"). Edits are more fixed kinds in `buildAdminSQL`:
`publication.options` (`ALTER PUBLICATION SET (publish, publish_via_partition_root)`,
the one change an all-tables publication allows), `subscription.publications`
(`SET PUBLICATION … WITH (refresh, copy_data)`; refresh needs it enabled) and
`subscription.options` (`SET (slot_name|NONE, synchronous_commit, streaming,
binary, disable_on_error)`). The subscription list carries binary/streaming
(PG14+) and disable_on_error (PG15+) as on/off strings, "" before; the
options form only sends what changed and hides what the server lacks.

## Finding databases, query tabs, saved queries, plans, diagram

- **Listing what a server has.** The agent's `dbdiscover` tunnel service
  (`internal/agent/dbdiscover.go`, grant field `Discover`) lists Postgres
  databases (pg-admin op `discover-databases`, owner and size), a Redis's
  logical dbs with key counts, or a RabbitMQ's vhosts (`rabbitmqctl
  list_vhosts`). It reads names only and never opens a workspace. The control
  plane serves it to admins at `GET /api/v1/assets/{id}/databases/discover`
  and marks entries already added; an agent too old to know the service is
  answered `state: "unsupported"` and the add form falls back to typing a
  name. Redis is one entry per server, so its picker is single-select.
- **Valkey is the redis engine** (2026-11). Same ACL users and RESP, so no
  new engine: the found app kind `valkey` adds a redis entry with config
  `/etc/valkey/valkey.conf`, and the agent falls back to that file when the
  entry has Redis's default path and no redis.conf exists (`redisConf`).
  `redisServerOf` reads `server_name`/`valkey_version` from INFO; the ready
  and discover answers carry `server: "valkey"` and the console names it
  Valkey. `ARGUS_TEST_REDIS_SERVER=valkey-server` runs every Redis agent test
  against Valkey.
- **Query tabs** (`web/src/lib/workspaceTabs.ts`): up to 10 per database,
  text kept in localStorage per database, results never stored. The session
  runs one statement at a time, so other tabs queue ("Run after X"). Ctrl+T
  and Ctrl+W cannot be taken from a browser; Ctrl+Enter runs, Ctrl+S saves.
- **Scripts (2026-11)**: the extended protocol refuses more than one
  statement ("cannot insert multiple commands into a prepared statement"),
  and that stays: one statement per request is what makes each recorded,
  guarded and checked on its own. So the console cuts a pasted script
  (`web/src/lib/sqlSplit.ts`: quotes, `$tag$` bodies, nested comments,
  parentheses, BEGIN ATOMIC) and runs it statement by statement on the same
  session, so BEGIN, temp tables and `pg_temp` functions last until the
  script ends them. It stops at the first failure, refusal or declined
  dialog, so a COMMIT after a failure is never sent, and says when a
  transaction is still open. A write is checked as a single one would be;
  one whose check touched only `pg_temp_N.*` tables goes on unasked.
  `internal/agent/testdata/script.sql` and its `.statements.json` are read
  by both the TS splitter test and the agent's real-Postgres test.
- **Saved queries**: migration 043, `internal/control/saved_queries.go`.
  Private or shared with everyone who can open the same database; author
  edits, author or admin removes; audited `db.query_saved`/`db.query_removed`.
- **Explain** (`web/src/lib/explainPlan.ts`): asked in TEXT format, one line
  per row, so no cell reaches the 64KB cell limit. ANALYZE of a statement that
  may write runs inside BEGIN/ROLLBACK; inside the person's own open
  transaction (tracked by `txAfter`) it is refused instead.
- **Diagram** (`web/src/lib/erd.ts`, `SchemaDiagram.tsx`): three catalog
  statements run as the person's role, so it shows only what they may see.

## Check before it runs, and the guard (2026-11)

`internal/tunnel/sqlclass.go` reads a statement's shape (`ClassifySQL`: verb,
DROP object, targets, top-level WHERE, CASCADE, a data-changing CTE) from its
words, skipping comments, literals and dollar quotes. It is **not access
control**: enforcement stays Postgres grants. `GuardSQL` uses it to stop
accidents in the agent's `run`, before Postgres sees the statement:

- UPDATE/DELETE with no top-level WHERE → `AR001`; TRUNCATE and DROP TABLE /
  SCHEMA / VIEW / MATERIALIZED VIEW → `AR002`; DROP DATABASE → `AR003`, never
  runs from a session. The first two run when `PGRequest.Confirm` equals the
  phrase `<db>/<targets>` the refusal carries (`PGAnswer.Guard`). The phrase
  is a pause for the person, not a secret.
- A refused statement is recorded as a `stmt` line plus an `error` line with
  the guard code, so the replay shows the attempt.
- Known gaps, by design (it says nothing when unsure): a DELETE inside a
  WITH, `WHERE true`, DROP of other object kinds, ALTER TABLE … DROP COLUMN.

Op `check` (`internal/agent/postgres_check.go`) runs an INSERT/UPDATE/DELETE/
MERGE as the person's role inside BEGIN…ROLLBACK (or a savepoint inside their
own transaction; refused in a failed one), and answers the tag's count plus
every table touched (cascades, triggers) from `pg_stat_xact_user_tables`
before/after. Recorded as a `check` line and a `done` line tagged
`ROLLED BACK …`. Sequences still advance; the console says so.

The console mirrors the classifier in `web/src/lib/sqlClass.ts`; both test
against `internal/tunnel/testdata/sqlclass.json`. `WriteCheck.tsx` is the
dialog: the count before UPDATE/DELETE/MERGE/INSERT…SELECT, and the guard's
reason, size and `pg_depend` dependents before a DROP/TRUNCATE. An agent
without `check` answers "unknown operation"; the console then runs as before
and the agent's guard (if any) still applies.

- The Owner level role is granted `CREATE ON DATABASE` (pgEnsureLevels), so an
  Owner session can create schemas owned by the level. See database-create.md.
