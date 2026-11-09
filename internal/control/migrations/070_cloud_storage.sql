-- Cloud, phase 3: disks, snapshots, images and Elastic IPs go through the
-- same change path as instances.
ALTER TABLE cloud_changes DROP CONSTRAINT IF EXISTS cloud_changes_action_check;
ALTER TABLE cloud_changes ADD CONSTRAINT cloud_changes_action_check CHECK (action IN (
    'launch', 'start', 'stop', 'reboot', 'resize', 'tags', 'protect', 'unprotect', 'remove',
    'volume-create', 'volume-grow', 'volume-attach', 'volume-detach', 'volume-delete',
    'snapshot-create', 'snapshot-delete', 'image-create', 'image-delete',
    'address-allocate', 'address-associate', 'address-disassociate', 'address-release'));

-- The manage permissions an account's manage role lacks, read from the
-- role's own policy at each sync, and when that was last read (NULL: not
-- yet, or the policy could not be read).
ALTER TABLE cloud_accounts ADD COLUMN IF NOT EXISTS manage_missing TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE cloud_accounts ADD COLUMN IF NOT EXISTS manage_checked_at TIMESTAMPTZ;
