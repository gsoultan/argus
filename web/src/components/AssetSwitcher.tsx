import { useQuery } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { ItemSwitcher } from '~/components/ItemSwitcher'
import { sortAssets } from '~/components/myservers'
import type { ServerTab } from '~/components/server/tabs'
import { assetShortName } from '~/lib/assetType'
import { assetsQuery } from '~/lib/queries'

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
  const items = sortAssets(data ?? []).map((a) => ({ value: a.id, label: `${assetShortName(a)} · ${a.address}` }))
  return (
    <ItemSwitcher what="asset" items={items} currentId={currentId}
      onGo={(id) => void navigate({ to: '/assets/$assetId', params: { assetId: id }, search: tab && tab !== 'overview' ? { tab } : {} })} />
  )
}
