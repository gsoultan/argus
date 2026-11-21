-- What a website check sends: a method, headers and a body, for endpoints
-- that only answer a request with something in it (a health API that wants
-- a POST, a key in a header).
--
-- The headers' values are sealed by the console (AES-GCM, a key derived
-- from signing_secret, as Telegram bot tokens are) because they are where an
-- API key goes; only their names are kept in the clear, to show which are
-- set. The body is kept as written: the form says to put secrets in a header.
ALTER TABLE website_checks ADD COLUMN IF NOT EXISTS method TEXT NOT NULL DEFAULT 'GET';
ALTER TABLE website_checks DROP CONSTRAINT IF EXISTS website_checks_method_check;
ALTER TABLE website_checks ADD CONSTRAINT website_checks_method_check
    CHECK (method IN ('GET', 'HEAD', 'POST', 'PUT', 'PATCH'));
ALTER TABLE website_checks ADD COLUMN IF NOT EXISTS request_body TEXT NOT NULL DEFAULT '';
ALTER TABLE website_checks DROP CONSTRAINT IF EXISTS website_checks_request_body_check;
ALTER TABLE website_checks ADD CONSTRAINT website_checks_request_body_check
    CHECK (length(request_body) <= 65536);
ALTER TABLE website_checks ADD COLUMN IF NOT EXISTS header_names TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE website_checks DROP CONSTRAINT IF EXISTS website_checks_header_names_check;
ALTER TABLE website_checks ADD CONSTRAINT website_checks_header_names_check
    CHECK (cardinality(header_names) <= 20);
ALTER TABLE website_checks ADD COLUMN IF NOT EXISTS headers_sealed BYTEA;
