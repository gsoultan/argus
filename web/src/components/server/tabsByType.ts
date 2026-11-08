import { assetTypeOf, type AssetType } from '~/lib/assetType'
import type { AssetKind } from '~/types/domain'
import { SERVER_TABS, type ServerTab } from '~/components/server/tabs'

/** Which tabs each type of asset has. */
export const TABS_BY_TYPE: Record<AssetType, readonly ServerTab[]> = {
  server: SERVER_TABS,
  // Log cleanup and growing disks are for servers; on someone's laptop they
  // are not ours to do.
  workstation: ['overview', 'access', 'sessions', 'apps', 'updates', 'agent', 'settings'],
  // Android updates itself, manages its own storage and has no host logs.
  device: ['overview', 'access', 'sessions', 'apps', 'agent', 'settings'],
}

/** A tab's name on an asset of this kind. */
export function tabLabel(tab: ServerTab, kind?: AssetKind): string {
  const type = assetTypeOf({ kind })
  switch (tab) {
    case 'overview': return 'Overview'
    case 'access': return 'Access'
    case 'sessions': return 'Sessions'
    case 'apps': return 'Apps'
    // A server's packages are the work; a workstation gets its updates.
    case 'updates': return type === 'server' ? 'Packages' : 'Updates'
    case 'disks': return 'Disks'
    case 'logs': return 'Logs'
    case 'agent': return type === 'device' ? 'App on device' : 'Agent'
    case 'settings': return 'Settings'
  }
}
