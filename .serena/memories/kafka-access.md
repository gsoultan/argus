# Kafka access (2026-11)

Engine `kafka`, shown under **Brokers** beside RabbitMQ (`isBroker()` in
`web/src/lib/engines.ts`). One entry = the topics one scope names on one
cluster: `*` for every topic, or a prefix such as `orders.` (dbname holds it,
max 63 chars). Code: `internal/tunnel/kafka.go` (scope, levels, admin file),
`internal/agent/kafka.go` (session, franz-go/kadm),
`web/src/components/kafka/KafkaWorkspace.tsx`, `web/src/lib/kafka.ts`.
Migration 054 widens the engine CHECK to every engine Argus knows.

- **Store frames**, like Redis/RabbitMQ. Not recorded: `overview`, `topics`,
  `topic` (partitions, offsets, non-default configs, group lag), `groups`.
  Recorded before they run: `peek` (newest N, no consumer group, nothing
  committed), `publish` (key = routingKey, headers, value = payload),
  `create_topic` (count = partitions), `delete_topic` (confirm = host name).
  Any topic outside the scope is refused before Kafka is asked; internal
  topics are never listed.
- **Admin sign-in** from a root-only (mode 600) `key = value` file on the
  server, default `/etc/argus/kafka.conf` (user, password, mechanism
  SCRAM-SHA-512 default or SCRAM-SHA-256), over SASL on a loopback listener.
  A cluster whose DescribeACLs answers SECURITY_DISABLED (no authorizer) is
  refused.
- **Per-person users** `argus-<login>-<level>`: SCRAM-SHA-512 credential via
  AlterUserSCRAMs, one-time password shared by concurrent sessions
  (userUse.password), ACLs on the scope (`*` LITERAL or prefix PREFIXED) with
  `tunnel.KafkaTopicOps(level)` plus DESCRIBE on every group. At last close
  the credential and every ACL naming the principal are deleted; stale argus-
  users are swept at open. Levels: read = READ/DESCRIBE/DESCRIBE_CONFIGS,
  write + WRITE, owner/dba + CREATE/DELETE/ALTER/ALTER_CONFIGS. Nothing on the
  cluster resource.
- **Kafka filters need `.Operations()`** (no args = ANY) on a kadm
  ACLBuilder used for describe/delete, or the filter matches nothing.
- **Testing.** Live tests need `ARGUS_TEST_KAFKA_PORT` (SASL_PLAINTEXT
  listener with an authorizer) and `ARGUS_TEST_KAFKA_ADMIN` (600 file with a
  SCRAM super user).
