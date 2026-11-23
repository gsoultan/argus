import { assetTypeOf, type AssetType } from '~/lib/assetType'
import type { AssetKind } from '~/types/domain'
import { SERVER_TABS, type ServerTab } from '~/components/server/tabs'

/** Which tabs each type of asset has. */
export const TABS_BY_TYPE: Record<AssetType, readonly ServerTab[]> = {
  server: SERVER_TABS,
  // Log cleanup and growing disks are for servers; on someone's laptop they
  // are not ours to do.
  // A workstation's own firewall is worth seeing too.
  workstation: ['overview', 'metrics', 'access', 'sessions', 'apps', 'software', 'updates', 'firewall', 'agent', 'settings'],
  // Android updates itself, manages its own storage and has no host logs;
  // its app reports no processor, memory or disk figures, so no Metrics.
  device: ['overview', 'access', 'sessions', 'apps', 'agent', 'settings'],
}

/** A tab's name on an asset of this kind. */
export function tabLabel(tab: ServerTab, kind?: AssetKind): string {
  const type = assetTypeOf({ kind })
  switch (tab) {
    case 'overview': return 'Overview'
    case 'metrics': return 'Metrics'
    case 'access': return 'Access'
    case 'sessions': return 'Sessions'
    case 'apps': return 'Apps'
    // Installing and removing software has a tab of its own; Updates is
    // the operating system's updates and release upgrades, on every type.
    case 'software': return 'Software'
    case 'updates': return 'Updates'
    case 'disks': return 'Disks'
    case 'firewall': return 'Firewall'
    case 'logs': return 'Logs'
    case 'agent': return type === 'device' ? 'App on device' : 'Agent'
    case 'settings': return 'Settings'
  }
}
