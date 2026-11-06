-- MySQL and MariaDB are one more engine of a database, one entry per
-- database on the server. Everything around a database -- levels, approvals,
-- sessions and their recordings -- already names it by id, so only the
-- engine check changes. Widening a CHECK never fails on rows that exist.
ALTER TABLE databases DROP CONSTRAINT IF EXISTS databases_engine_check;
ALTER TABLE databases ADD CONSTRAINT databases_engine_check
    CHECK (engine IN ('postgres', 'redis', 'rabbitmq', 'mysql'));
