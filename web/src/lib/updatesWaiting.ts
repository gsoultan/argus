import { get } from '~/lib/live'

/*
 * What is waiting to be updated across the fleet: the Updates page, the
 * assets list's Needs updates filter and its badges. Kept out of live.ts,
 * which every page loads.
 */

export interface UpdatesWaitingRow {
  assetId: string
  hostname: string
  updates: number
  security: number
  rebootRequired: boolean
  /** behind, updating, held, cannot or too_old; absent when current. */
  agent?: 'behind' | 'updating' | 'held' | 'cannot' | 'too_old'
  agentVersion?: string
}

export interface UpdatesWaiting {
  latest: string
  assets: UpdatesWaitingRow[]
  securityAssets: number
  securityFixes: number
  updateAssets: number
  rebootAssets: number
  agentsBehind: number
}

export const updatesWaitingQuery = () => ({
  queryKey: ['updates-waiting'] as const,
  queryFn: (): Promise<UpdatesWaiting> => get('/api/v1/updates-waiting'),
  staleTime: 60_000,
})

/** Whether an asset has anything waiting worth a badge. */
export function needsUpdates(r: UpdatesWaitingRow | undefined): boolean {
  return !!r && (r.security > 0 || r.updates > 0 || r.rebootRequired || r.agent === 'behind')
}
