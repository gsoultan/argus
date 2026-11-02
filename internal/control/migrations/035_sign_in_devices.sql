-- Browsers and addresses that have signed in to each account, so a sign-in
-- alert can say "new device" and, by default, be sent only then.
--
-- fingerprint is a SHA-256 of the client address (an IPv6 address by its /64)
-- and the browser and operating system named by the User-Agent, without the
-- version: a browser updating itself is not a new device. Nothing here is
-- evidence of anything; the audit log holds the sign-ins themselves. Rows
-- not seen for 90 days are deleted at the account's next sign-in, so the
-- table is bounded by accounts times the devices each uses.
CREATE TABLE IF NOT EXISTS sign_in_devices (
    email         TEXT        NOT NULL,
    fingerprint   TEXT        NOT NULL,
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (email, fingerprint)
);
