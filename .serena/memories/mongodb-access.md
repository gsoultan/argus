# MongoDB access (2026-11)

Engine `mongodb`, one entry = one database on one local mongod. Code:
`internal/tunnel/mongodb.go` (names, roles, admin file), `internal/agent/mongodb.go`
(session), `internal/agent/mongoshell.go` (command line parser),
`web/src/components/MongoWorkspace.tsx`. Migration 053 widens the engine CHECK
to every engine Argus knows, so engine migrations are order-independent.

- **Store frames**, like Redis/RabbitMQ: StoreRequest ops `run`, `scan`
  (collections + estimated counts, not recorded), `value` (first documents of
  a collection, recorded as `db.getCollection("x").find({}).limit(n)`).
  Answers carry documents in `Data` as relaxed Extended JSON and a `Tag`
  ("find 12", "update 3 matched 3 changed").
- **Admin sign-in** from a root-only (mode 600) `key = value` file on the
  server, default `/etc/argus/mongodb.conf` (user, password, authSource,
  authMechanism). Never sent anywhere. A mongod that lists databases without
  auth (no access control) is refused. MongoDB 5.0+.
- **Per-person users** `argus-<login>-<level>` created in the entry's
  database, SCRAM-SHA-256, one-time password shared by concurrent sessions
  (userUse.password), dropped at last close; stale argus- users in that db are
  swept at open. Roles: read=read, write=readWrite, owner=readWrite+dbAdmin,
  dba=dbOwner+clusterMonitor(admin).
- **Command line** reads mongosh-shaped literals only (no JS): db.c.find(...)
  with .sort/.limit/.skip, findOne, countDocuments, aggregate, insert*/update*/
  replaceOne/delete*, createIndex, drop, db.runCommand, bare `{...}`. Each
  becomes ONE command document. Confirm by server name: drop, dropDatabase,
  deleteMany({}), updateMany({}). Refused: changing argus- users,
  dropAllUsersFromDatabase. Reads are capped at maxRows+1 (singleBatch /
  cursor batchSize, leftover cursors killed) to flag truncation.
- **Testing.** Live tests need `ARGUS_TEST_MONGODB_PORT` and
  `ARGUS_TEST_MONGODB_ADMIN` (600 file with a root account).

## Performance analysis (2026-11)

- **Op `perf`** (tunnel.MongoOpPerf): read-only, DBA only. Refused below DBA
  by both the relay (store_network.go storeNetworkGate) and the agent
  (mongo_perf.go). Listed in ready.ops only at DBA; the workspace shows a
  "Performance" button when ops include it.
- **Agent read** (internal/agent/mongo_perf.go, readMongoPerf): serverStatus
  trimmed to uptime, connections, opcounters, globalLock, mem, wiredTiger
  cache/tickets, metrics, opLatencies; dbStats; per collection (max 200,
  system.* skipped) $collStats storageStats + $indexStats (spec reduced to
  unique/ttl/partial); profiler level and, if on, system.profile grouped by
  ns/op/plan/queryHash (excluding the panel's own $collStats/$indexStats);
  $currentOp ops >= 5 s in this database. Each source fails on its own into
  Missing. The v2 driver decodes nested docs in bson.M as bson.D: use asM.
- **Analysis** (web/src/lib/mongoPerformance.ts, analyzeMongo): connections,
  queueing, cache full/dirty, examined vs returned, in-memory sorts, write
  conflicts, cursor timeouts, profiler off/all, heavy profiled shapes with an
  ESR createIndex, redundant prefix indexes, unused (7 days, hide first),
  oversized indexes, free space (compact), long operations. Setup advice sorts
  after other findings of the same severity.
- **Panel** (web/src/components/mongo/MongoPerformance.tsx): a modal over the
  console; fixes go to the command line ("Put on the command line") and are
  never run. FindingsList takes openLabel/statement for this.
- **Test**: TestMongoDBPerfOnARealServer (non-Windows build tag, like every
  live Mongo test); fixture web/src/lib/mongoPerformance.fixture.json is a
  real MongoDB 7 capture (ARGUS_TEST_MONGODB_PERF_OUT writes it).
