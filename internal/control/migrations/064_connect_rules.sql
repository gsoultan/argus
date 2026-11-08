-- Who is asked before a session opens, per type of asset, and an asset's
-- own setting where it differs from its type's.
--
-- gateway_policy.connect_rules holds the rules per type as an object
-- ({"server": {...}, "workstation": {...}}); a type it does not name follows
-- the built-in default (control/connect_rules.go), so '{}' is the default.
-- assets.connect_ask is NULL to follow the type, or the asset's own answer.
--
-- Only added columns: nothing to rewrite on a populated database.

ALTER TABLE gateway_policy ADD COLUMN IF NOT EXISTS connect_rules JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE gateway_policy DROP CONSTRAINT IF EXISTS gateway_policy_connect_rules_check;
ALTER TABLE gateway_policy ADD CONSTRAINT gateway_policy_connect_rules_check
    CHECK (jsonb_typeof(connect_rules) = 'object');

ALTER TABLE assets ADD COLUMN IF NOT EXISTS connect_ask TEXT;
ALTER TABLE assets DROP CONSTRAINT IF EXISTS assets_connect_ask_check;
ALTER TABLE assets ADD CONSTRAINT assets_connect_ask_check
    CHECK (connect_ask IS NULL OR connect_ask IN ('never', 'ask', 'tell'));
