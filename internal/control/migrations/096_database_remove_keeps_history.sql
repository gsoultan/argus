-- Removing a database from Argus keeps its sessions and access requests, as
-- the console says. Both pointed at it with a plain foreign key, so once
-- anyone had opened the database, or asked to, the DELETE broke that key and
-- the removal failed. They now let go of the link and keep everything else:
-- a session still names its server, and its recording stays where it is.
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_database_id_fkey;
ALTER TABLE sessions ADD CONSTRAINT sessions_database_id_fkey
    FOREIGN KEY (database_id) REFERENCES databases(id) ON DELETE SET NULL;

ALTER TABLE access_requests DROP CONSTRAINT IF EXISTS access_requests_database_id_fkey;
ALTER TABLE access_requests ADD CONSTRAINT access_requests_database_id_fkey
    FOREIGN KEY (database_id) REFERENCES databases(id) ON DELETE SET NULL;
