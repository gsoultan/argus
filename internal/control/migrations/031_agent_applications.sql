-- What runs on each server: databases, brokers, web servers, containers.
--
-- The agent reports it with its host facts: listening ports and the process
-- behind each, running services, known packages and containers. Names,
-- versions, ports and addresses only -- never a command line, an environment
-- or a configuration file. NULL until an agent new enough to look reports,
-- so the console can say "not reported" rather than "nothing runs here".
ALTER TABLE agents ADD COLUMN IF NOT EXISTS applications JSONB;
