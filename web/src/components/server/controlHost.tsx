import { Alert, Badge, Text } from '@mantine/core'
import { IconServerBolt } from '@tabler/icons-react'
import { FS } from '~/theme'

/**
 * argus-control's own server: looked after through the helper the
 * argus-control package ships, never an agent someone installed. Said in the
 * header, and in Settings where the difference matters.
 */
export const CONTROL_HOST_SSH =
  'Plain SSH to this server is not gated or recorded by Argus, on purpose: it is how you get back in when Argus itself is down. Sessions opened from the console are recorded as on any server.'

export function ControlHostBadge() {
  return (
    <Badge size="sm" variant="light" color="slate" radius="sm" tt="none" data-testid="control-host-badge"
      title={`argus-control runs on this server. ${CONTROL_HOST_SSH}`}>
      argus-control&rsquo;s server
    </Badge>
  )
}

/** Why this server has no Remove button, in its place. */
export function ControlHostRemove() {
  return (
    <Alert variant="light" color="slate" icon={<IconServerBolt size={16} />} data-testid="control-host-remove">
      <Text size={FS.meta}>
        argus-control runs on this server and looks after it through the helper its package installs, so it cannot be
        removed from Argus here. To stop managing it from the console, set <code>control_host: false</code> in
        argus-control&rsquo;s configuration, then revoke its agent under Settings.
      </Text>
    </Alert>
  )
}
