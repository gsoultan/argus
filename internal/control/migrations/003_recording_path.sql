-- Where the sealed recording lives in object storage. NULL means it never left
-- the host that produced it, which the console surfaces as a weaker guarantee.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS recording_key TEXT;
