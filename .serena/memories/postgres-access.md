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
