# NATS access (2026-11)

Engine `nats`, shown under **Brokers** beside RabbitMQ and Kafka
(`isBroker()`, `BROKER_ENGINES` in `web/src/lib/engines.ts`, mark "Na").
One entry = one NATS **account** on one server (dbname holds the account
name; `SYS`/`$SYS` refused). Operator mode only: a server anyone can use
without signing in is refused at open and at discovery. Code:
`internal/tunnel/nats.go` (names, subjects, levels, permissions, ops, specs),
`internal/agent/nats.go` (session, nats.go + jetstream), `web/src/lib/nats.ts`,
`web/src/components/nats/` (NatsWorkspace, NatsStream, NatsForms).
Migration 115 widens the engine CHECK.

- **Signing key file** on the server, root-only (mode 600), default
  `/etc/argus/nats.conf` (configPath): one line
  `account NAME = ACCOUNT_PUBLIC_KEY SIGNING_KEY_SEED` per account, optional
  `monitor = PORT` (default 8222). Discovery lists the accounts in it.
- **Per-session user**: the agent mints a user JWT (jwt/v2 + nkeys) signed
  with the account's signing key, IssuerAccount set, named
  `argus-<login>-<level>`, expiring at max session + 5 min, with a private
  inbox `_INBOX_argus_<hex>`. Nothing is created on the server and there is
  nothing to clean up. Connection name starts `argus · ` so Argus's own
  sessions are flagged in Connections and left out of Subjects.
- **Levels as NATS permissions** (`tunnel.NATSPermissions`): read = only the
  read JetStream API (INFO/LIST/NAMES/DIRECT.GET/MSG.GET); write = publish
  `>` minus stream/consumer management API; Manage/Admin = `>` minus
  `$SYS` and the like. Subscribe `>` minus `$SYS`, `_INBOX.>`, `$JS.ACK`,
  `$JS.FC`. NATS itself enforces these; tests prove it on an embedded
  operator-mode server (`internal/agent/nats_test.go`).
- **Store frames** carry `nats: {stream, consumer, subject, from, seq, time,
  mode, seconds, keep, streamSpec, consumerSpec}`. Not recorded: overview,
  streams, stream, consumers, consumer, subjects, connections. Recorded
  before running: get (direct get by sequence: no consumer, no ack), tail
  (core subscribe, at most 30 s / 200), publish (core | stream | request),
  stream_create/update/purge/delete, msg_delete, consumer_create/delete/
  pause/move. stream_delete and a whole-stream purge need `confirm` = the
  stream name. consumer_move deletes and remakes a durable consumer.
- **Monitoring port** (`/accstatz`, `/connz?acc=`) gives connections, rates
  and subscriptions; when it does not answer, the rest still works.
- Later PRs planned: key-value + Network tab; NATS users for apps (.creds).
