# Creating databases, vhosts and topics from Argus

- Tunnel service `dbcreate` (frame `'c'`, `internal/tunnel/dbcreate.go`): ops
  `create`, `options`, `set-owner`. `GrantCreate.Validate` is the one gate for
  every field; `CreatePlan`/`SetOwnerPlan` build the statements, so the
  control plane's dry run (preview) is exactly what the agent runs.
- Agent (`internal/agent/dbcreate.go`, `postgres_create.go`): Postgres goes
  through the pg-admin helper as postgres (create-database in `postgres`, then
  prepare-new-database inside the new one: level roles + extensions). MySQL
  via `adminConn`, MongoDB creates the first collection (a database exists
  only with one), RabbitMQ `rabbitmqctl add_vhost` + `set_vhost_limits`, Kafka
  kadm `CreateTopic` with configs. Redis has nothing to create (refused).
- Never runs over a taken name: answers `exists`, and the UI offers "Add X
  instead". Loopback/socket hosts only. `argus_test_` names are refused.
- Postgres default owner is Argus's Owner level role (`tunnel.PGLevelRole`,
  mirrored in `web/src/lib/pgRoles.ts` — keep them identical). The Owner level
  has `CREATE ON DATABASE`, so Owner sessions can `CREATE SCHEMA … AUTHORIZATION
  <owner level>` (New schema in the SQL tree).
- Owner change: `POST /api/v1/databases/{id}/owner` → `ALTER DATABASE … OWNER
  TO`. No REASSIGN OWNED (too broad). Audited `db.owner` / `db.owner.failed`.
- Control (`internal/control/database_create.go`): `GET
  /assets/{id}/databases/create-options`, `POST /assets/{id}/databases/create`
  (dryRun supported), admin/owner role only. Audits `db.created` (with the
  statements) or `db.create.failed`, then adds the entry (`db.added`) and
  applies the server's rules. A Kafka topic already covered by an entry's
  scope adds no entry unless `addEntry`.
- Old agents answer "unknown service" → state `unsupported`.
- Inside-object forms (web only, through existing session ops so they are
  recorded like typed commands): RabbitMQ New queue (classic/quorum/stream via
  `StoreRequest.queue`, `tunnel.RabbitQueue.Arguments`), Kafka New topic
  (properties replication/retention/cleanup), MongoDB New collection/index
  (shell lines, `mongo/MongoNew.tsx`), Redis New key (`redis/RedisNewKey.tsx`,
  EXISTS check first, SET … NX), Postgres New schema (`SqlGenerate.tsx`).
