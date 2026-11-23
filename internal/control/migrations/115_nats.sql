-- NATS is one more engine under Brokers: one entry per NATS account on a
-- server, as one RabbitMQ entry is one vhost. Everything around an entry --
-- levels, approvals, sessions and their recordings -- already names it by
-- id, so only the engine check changes. The list is every engine Argus
-- knows. Widening a CHECK never fails on rows that exist.
ALTER TABLE databases DROP CONSTRAINT IF EXISTS databases_engine_check;
ALTER TABLE databases ADD CONSTRAINT databases_engine_check
    CHECK (engine IN ('postgres', 'redis', 'rabbitmq', 'mysql', 'mongodb', 'kafka', 'mssql', 'nats'));
