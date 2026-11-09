-- Cloud, phase 4: security groups and load balancer targets go through the
-- same change path as instances and disks.
ALTER TABLE cloud_changes DROP CONSTRAINT IF EXISTS cloud_changes_action_check;
ALTER TABLE cloud_changes ADD CONSTRAINT cloud_changes_action_check CHECK (action IN (
    'launch', 'start', 'stop', 'reboot', 'resize', 'tags', 'protect', 'unprotect', 'remove',
    'volume-create', 'volume-grow', 'volume-attach', 'volume-detach', 'volume-delete',
    'snapshot-create', 'snapshot-delete', 'image-create', 'image-delete',
    'address-allocate', 'address-associate', 'address-disassociate', 'address-release',
    'firewall-create', 'firewall-rules', 'firewall-delete', 'instance-firewalls',
    'target-register', 'target-deregister'));
