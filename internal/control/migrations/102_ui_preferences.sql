-- How each person likes the console laid out (ui_preferences.go): cards or a
-- table on a list page, the sidebar open or folded to icons. One row per
-- person and setting. A new table, so an older version ignores it.
CREATE TABLE IF NOT EXISTS ui_preferences (
    email      TEXT NOT NULL,
    key        TEXT NOT NULL CHECK (length(key) BETWEEN 1 AND 64),
    value      TEXT NOT NULL CHECK (length(value) BETWEEN 1 AND 32),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (email, key)
);
