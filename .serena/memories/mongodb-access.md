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
