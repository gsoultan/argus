-- An organization's own Telegram bots (telegram_bots.go), and which bot each
-- kind of alert uses. New tables and one new column, so an older version
-- ignores them.
CREATE TABLE IF NOT EXISTS telegram_bots (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 80),
    -- @username, from Telegram when the token was checked.
    username      TEXT NOT NULL DEFAULT '',
    -- The token, sealed with a key derived from signing_secret; never
    -- returned. token_tail is its last three characters, to tell bots apart.
    token_sealed  BYTEA NOT NULL,
    token_tail    TEXT NOT NULL DEFAULT '',
    -- The chats it may post to: [{"id","title","kind"}]. It never posts
    -- anywhere else.
    chats         JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(chats) = 'array'),
    -- The organization's default bot: alerts left on "default bot" use it.
    is_default    BOOLEAN NOT NULL DEFAULT false,
    last_ok_at    TIMESTAMPTZ,
    last_error    TEXT NOT NULL DEFAULT '',
    last_error_at TIMESTAMPTZ,
    created_by    TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- At most one default.
CREATE UNIQUE INDEX IF NOT EXISTS telegram_bots_one_default ON telegram_bots (is_default) WHERE is_default;

-- Which bot and chats each kind of alert uses. A kind with no row uses the
-- default bot and all of its chats. bot: '' is the default bot, 'off' is no
-- Telegram, 'platform' is the config file's bot, else a telegram_bots id.
CREATE TABLE IF NOT EXISTS telegram_routes (
    kind       TEXT PRIMARY KEY CHECK (kind IN ('signin', 'server', 'website', 'backup')),
    bot        TEXT NOT NULL DEFAULT '' CHECK (length(bot) <= 40),
    chats      TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(chats) <= 20),
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The Websites setting moves here from website_alert_settings, which is
-- kept, unused, so the previous version still finds it.
INSERT INTO telegram_routes (kind, bot, chats, updated_by, updated_at)
-- Chats chosen there were the config file's bot's, so they stay with it.
SELECT 'website',
       CASE WHEN NOT telegram THEN 'off' WHEN cardinality(telegram_chats) > 0 THEN 'platform' ELSE '' END,
       telegram_chats, updated_by, updated_at
FROM website_alert_settings
ON CONFLICT (kind) DO NOTHING;

-- A website with its own chats also names its bot: '' the default bot,
-- 'platform', or a telegram_bots id.
ALTER TABLE website_checks ADD COLUMN IF NOT EXISTS telegram_bot TEXT NOT NULL DEFAULT '' CHECK (length(telegram_bot) <= 40);
-- Sites that chose chats chose the config file's bot's.
UPDATE website_checks SET telegram_bot = 'platform' WHERE telegram = 'chats' AND telegram_bot = '';
