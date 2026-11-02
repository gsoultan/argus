-- Redis and RabbitMQ are two more engines of a database. Everything around a
-- database -- who may open it at which level, approvals, sessions and their
-- recordings -- already names a database by id, so nothing else changes:
-- the engine column was added for this.
--
-- One entry is one Redis server (dbname is the logical db it opens in) or one
-- RabbitMQ vhost (dbname is the vhost; host and port are the management API).
-- As for Postgres, the address is on the server itself and no password is
-- kept: the agent signs in as the engine's administrator there.
ALTER TABLE databases DROP CONSTRAINT IF EXISTS databases_engine_check;
ALTER TABLE databases ADD CONSTRAINT databases_engine_check
    CHECK (engine IN ('postgres', 'redis', 'rabbitmq'));

-- The server's redis.conf, which the agent reads for the admin password.
-- A path on the server, never the password.
ALTER TABLE databases ADD COLUMN IF NOT EXISTS config_path TEXT NOT NULL DEFAULT '';

-- Redis's ACL is per server, not per logical db, so two entries for one
-- Redis would be two answers to who may reach it.
CREATE UNIQUE INDEX IF NOT EXISTS databases_redis_server_key
    ON databases (asset_id, port) WHERE engine = 'redis';
