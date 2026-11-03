import { Alert, Button, Group, Text } from '@mantine/core'
import { IconAlertTriangle } from '@tabler/icons-react'
import { run } from '~/lib/notify'
import { usePinHostKey } from '~/lib/queries'
import { FS } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * Pinning the key a server presents now.
 *
 * Guarded. Pinning is the control an admin reaches for when they suspect
 * interception, so a refusal that looked like success would be the worst
 * possible moment to leave someone guessing.
 */
export function usePin(asset: Asset) {
  const pin = usePinHostKey()
  const onPin = () =>
    run(() => pin.mutateAsync(asset.id), {
      failure: 'Host key not pinned',
      success: [
        'Host key pinned',
        'Argus will now refuse any connection where the presented key differs.',
      ],
    })
  return { onPin, pending: pin.isPending }
}

/**
 * The presented key no longer matches the pin, so every connection is being
 * refused. Shown on every tab: it is the reason Connect does nothing.
 */
export function HostKeyChanged({ asset }: { asset: Asset }) {
  const { onPin, pending } = usePin(asset)
  if (asset.hostKeyState !== 'changed') return null
  return (
    <Alert color="rose" icon={<IconAlertTriangle size={17} />} title="Host key does not match the pin">
      <Text size={FS.body} mb="sm" lh={1.5}>
        Argus is refusing connections to this host. Either it was rebuilt or reprovisioned, or
        something is intercepting the connection. Verify the new fingerprint out of band (console
        access, your provisioning system, or a colleague physically at the host) before re-pinning.
        Do not re-pin just to clear the alert.
      </Text>
      <Button size="compact-xs" color="rose" loading={pending} onClick={() => void onPin()}>
        I verified out of band — re-pin
      </Button>
    </Alert>
  )
}

/**
 * No key is pinned yet: one line with its fix beside it. The longer
 * explanation is on the Settings tab, under Host key.
 */
export function HostKeyUnpinned({ asset }: { asset: Asset }) {
  const { onPin, pending } = usePin(asset)
  if (asset.hostKeyState !== 'unpinned') return null
  return (
    <Alert color="amber" variant="light" p="xs" icon={<IconAlertTriangle size={16} />}>
      <Group justify="space-between" gap="xs" wrap="wrap">
        <Text size={FS.body} lh={1.45} style={{ flex: '1 1 260px' }}>
          <Text span fw={600} inherit>No host key pinned.</Text>{' '}
          Until one is, the first connection trusts whatever key the server shows.
        </Text>
        <Button size="compact-sm" color="amber" loading={pending} onClick={() => void onPin()}>
          Pin current key
        </Button>
      </Group>
    </Alert>
  )
}
