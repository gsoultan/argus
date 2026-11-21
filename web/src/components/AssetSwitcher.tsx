import { ActionIcon, Group, Select, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { IconChevronLeft, IconChevronRight } from '@tabler/icons-react'
import { sortAssets } from '~/components/myservers'
import { assetShortName } from '~/lib/assetType'
import { assetsQuery } from '~/lib/queries'
import type { ServerTab } from '~/components/server/tabs'

/**
 * Moving between assets without leaving the page: previous, next, and a
 * search of every asset by name or address. It replaces the list that sat
 * beside the page, so Assets has one list and one asset page, full width.
 *
 * The order is the Assets list's (blocked, offline, then by name), and the
 * tab stays where it is, so stepping through Updates on every server is one
 * click each.
 */
export function AssetSwitcher({ currentId, tab }: { currentId: string; tab?: ServerTab }) {
  const navigate = useNavigate()
  const { data } = useQuery(assetsQuery())
  const sorted = sortAssets(data ?? [])
  const list = sorted.map((a) => ({ value: a.id, label: `${assetShortName(a)} · ${a.address}` }))
  const i = list.findIndex((a) => a.value === currentId)
  const go = (id?: string) =>
    id && void navigate({ to: '/assets/$assetId', params: { assetId: id }, search: tab && tab !== 'overview' ? { tab } : {} })
  if (list.length < 2) return null
  return (
    <Group gap={4} wrap="nowrap" data-testid="asset-switcher">
      <Tooltip label="Previous asset">
        <ActionIcon variant="default" size={36} disabled={i <= 0} onClick={() => go(list[i - 1]?.value)} aria-label="Previous asset">
          <IconChevronLeft size={16} />
        </ActionIcon>
      </Tooltip>
      <Select data={list} value={currentId} onChange={(v) => go(v ?? undefined)} searchable w={220} aria-label="Go to another asset"
        nothingFoundMessage="No asset by that name or address" comboboxProps={{ withinPortal: true }} />
      <Tooltip label="Next asset">
        <ActionIcon variant="default" size={36} disabled={i < 0 || i >= list.length - 1} onClick={() => go(list[i + 1]?.value)} aria-label="Next asset">
          <IconChevronRight size={16} />
        </ActionIcon>
      </Tooltip>
    </Group>
  )
}
