import { ActionIcon, Group, Select, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { IconChevronLeft, IconChevronRight } from '@tabler/icons-react'
import { assetShortName } from '~/lib/assetType'
import { assetsQuery } from '~/lib/queries'

/**
 * Moving between assets without leaving the page: previous, next, and a
 * search of every asset. It replaces the list that sat beside the page, so
 * Assets has one layout (the cards) and one asset page, full width.
 */
export function AssetSwitcher({ currentId }: { currentId: string }) {
  const navigate = useNavigate()
  const { data } = useQuery(assetsQuery())
  const list = (data ?? []).map((a) => ({ value: a.id, label: assetShortName(a) }))
  const i = list.findIndex((a) => a.value === currentId)
  const go = (id?: string) => id && void navigate({ to: '/assets/$assetId', params: { assetId: id } })
  return (
    <Group gap={4} wrap="nowrap">
      <Tooltip label="Previous asset"><ActionIcon variant="default" size={36} disabled={i <= 0} onClick={() => go(list[i - 1]?.value)} aria-label="Previous asset"><IconChevronLeft size={16} /></ActionIcon></Tooltip>
      <Select data={list} value={currentId} onChange={(v) => go(v ?? undefined)} searchable w={190} aria-label="Go to another asset"
        nothingFoundMessage="No asset by that name" comboboxProps={{ withinPortal: true }} />
      <Tooltip label="Next asset"><ActionIcon variant="default" size={36} disabled={i < 0 || i >= list.length - 1} onClick={() => go(list[i + 1]?.value)} aria-label="Next asset"><IconChevronRight size={16} /></ActionIcon></Tooltip>
    </Group>
  )
}
