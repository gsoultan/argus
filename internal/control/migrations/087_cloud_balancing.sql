-- Cloud, network: making target groups and load balancers, changing a
-- health check, and sizing Auto Scaling groups.
ALTER TABLE cloud_changes DROP CONSTRAINT IF EXISTS cloud_changes_action_check;
ALTER TABLE cloud_changes ADD CONSTRAINT cloud_changes_action_check CHECK (action IN (
    'launch', 'start', 'stop', 'hibernate', 'reboot', 'resize', 'tags', 'protect', 'unprotect', 'settings', 'remove',
    'volume-create', 'volume-grow', 'volume-attach', 'volume-detach', 'volume-delete',
    'snapshot-create', 'snapshot-delete', 'image-create', 'image-delete',
    'address-allocate', 'address-associate', 'address-disassociate', 'address-release',
    'firewall-create', 'firewall-rules', 'firewall-delete', 'instance-firewalls',
    'target-register', 'target-deregister', 'capacity-create', 'capacity-cancel', 'ri-buy', 'sp-buy',
    'target-group-create', 'health-check', 'balancer-create', 'group-size'));
