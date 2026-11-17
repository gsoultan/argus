-- The AWS side of a scale group (cloud/floor.go, scale_floor.go): the
-- launch template and Auto Scaling group under it, "AWS decides" as a
-- third mode, which members AWS started, and the group's enrollment key.
-- Only new columns, and one wider check.

ALTER TABLE scale_groups DROP CONSTRAINT IF EXISTS scale_groups_mode_check;
ALTER TABLE scale_groups ADD CONSTRAINT scale_groups_mode_check CHECK (mode IN ('ask', 'auto', 'aws'));

-- What AWS keeps the group near when it decides: {"metric", "target"}.
ALTER TABLE scale_groups ADD COLUMN IF NOT EXISTS aws_track JSONB NOT NULL DEFAULT '{}'::jsonb;
-- The Auto Scaling group and launch template, once made.
ALTER TABLE scale_groups ADD COLUMN IF NOT EXISTS aws_group TEXT NOT NULL DEFAULT '';
ALTER TABLE scale_groups ADD COLUMN IF NOT EXISTS aws_template TEXT NOT NULL DEFAULT '';
-- Why AWS refused to make them, and when Argus last tried.
ALTER TABLE scale_groups ADD COLUMN IF NOT EXISTS aws_note TEXT NOT NULL DEFAULT '';
ALTER TABLE scale_groups ADD COLUMN IF NOT EXISTS aws_tried_at TIMESTAMPTZ;

-- A member AWS started (and replaces, and ends) rather than Argus.
ALTER TABLE scale_group_members ADD COLUMN IF NOT EXISTS in_aws BOOLEAN NOT NULL DEFAULT false;

-- A scale group's key: one token in its launch template that every server
-- AWS starts enrolls with. Never used up; each use is checked against AWS
-- (scale_floor.go).
ALTER TABLE agent_enrollment_tokens ADD COLUMN IF NOT EXISTS scale_group_id UUID REFERENCES scale_groups (id) ON DELETE CASCADE;
CREATE INDEX IF NOT EXISTS agent_enrollment_tokens_scale_group ON agent_enrollment_tokens (scale_group_id) WHERE scale_group_id IS NOT NULL;
