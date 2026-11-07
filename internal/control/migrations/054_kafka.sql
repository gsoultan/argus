-- Kafka is one more engine of a database, shown under Brokers beside
-- RabbitMQ: one entry per scope of topics on a cluster ("*" or a prefix).
-- Everything around an entry -- levels, sessions and their recordings --
-- already names it by id, so only the engine check changes. The list is every
-- engine Argus knows, so it is the same whichever of the engine migrations
-- runs last. Widening a CHECK never fails on rows that exist.
ALTER TABLE databases DROP CONSTRAINT IF EXISTS databases_engine_check;
ALTER TABLE databases ADD CONSTRAINT databases_engine_check
    CHECK (engine IN ('postgres', 'redis', 'rabbitmq', 'mysql', 'mongodb', 'kafka'));
