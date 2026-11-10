import { Group, Popover, Stack, Text, UnstyledButton } from '@mantine/core'
import { addressesOf, isIpLiteral, type ShownAddress } from '~/lib/assetAddresses'
import type { Asset } from '~/types/domain'
import { FS, SP } from '~/theme'

/**
 * An asset's best address, and the rest behind "+N".
 *
 * The "+N" is a press, not a hover, so it opens on a phone too. It sits above
 * the card's stretched link (`argus-above`), so pressing it does not open the
 * asset.
 */
export function AssetAddresses({ asset, withEntered = false, size = FS.meta }: {
  asset: Pick<Asset, 'address' | 'addresses' | 'hostname'>
  /** Show the address an administrator entered when the agent did not report it. */
  withEntered?: boolean
  size?: string
}) {
  const all = addressesOf(asset, withEntered)
  if (all.length === 0) return null
  const [first, ...rest] = all
  return (
    <Group gap={SP.snug} wrap="nowrap" style={{ minWidth: 0 }} data-testid="asset-addresses">
      <Text span size={size} ff="monospace" c="dimmed" truncate title={note(first!)}>
        {first!.address}
      </Text>
      {rest.length > 0 && (
        <Popover position="bottom-start" shadow="md" withinPortal>
          <Popover.Target>
            <UnstyledButton
              className="argus-above"
              aria-label={`All ${all.length} addresses`}
              style={{ fontSize: 'var(--mantine-font-size-xs)', fontWeight: 600, color: 'var(--mantine-color-azure-6)', flexShrink: 0 }}
            >
              +{rest.length}
            </UnstyledButton>
          </Popover.Target>
          <Popover.Dropdown p="sm">
            <Stack gap={SP.tight} data-testid="asset-addresses-all">
              {all.map((a) => (
                <Group key={a.address} gap="sm" wrap="nowrap" justify="space-between">
                  <Text size={FS.meta} ff="monospace" style={{ overflowWrap: 'anywhere' }}>{a.address}</Text>
                  <Text size={FS.micro} c="dimmed" style={{ flexShrink: 0 }}>{note(a)}</Text>
                </Group>
              ))}
            </Stack>
          </Popover.Dropdown>
        </Popover>
      )}
    </Group>
  )
}

/** What kind of address it is, and where it came from. */
function note(a: ShownAddress): string {
  if (a.entered) return isIpLiteral(a.address) ? 'entered in Argus' : 'name entered in Argus'
  const kind = a.public ? 'public' : 'private'
  return a.interface ? `${kind} · ${a.interface}` : kind
}

