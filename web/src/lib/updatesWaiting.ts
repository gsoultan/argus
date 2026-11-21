import { get, live } from '~/lib/live'
import { api } from '~/lib/api'
import type { Asset } from '~/types/domain'

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
  /** The OS release it can be upgraded to (a major upgrade); absent when none. */
  releaseNext?: string
}

export interface UpdatesWaiting {
  latest: string
  assets: UpdatesWaitingRow[]
  securityAssets: number
  securityFixes: number
  updateAssets: number
  rebootAssets: number
  agentsBehind: number
  /** Assets that can move to a new OS release; absent from an older control plane. */
  releaseAssets?: number
}

export const updatesWaitingQuery = () => ({
  queryKey: ['updates-waiting'] as const,
  queryFn: (): Promise<UpdatesWaiting> =>
    live.orFallback(() => get<UpdatesWaiting>('/api/v1/updates-waiting'), async () => fixtureWaiting(await api.assets({}))),
  staleTime: 60_000,
})

/** Whether an asset has anything waiting worth a badge. */
export function needsUpdates(r: UpdatesWaitingRow | undefined): boolean {
  return !!r && (r.security > 0 || r.updates > 0 || r.rebootRequired || r.agent === 'behind' || !!r.releaseNext)
}

/** Made-up rows for the sample console, steady per asset so a reload agrees. */
function fixtureWaiting(assets: Asset[]): UpdatesWaiting {
  const latest = '1.42.0'
  const rows: UpdatesWaitingRow[] = []
  for (const a of assets) {
    if (a.agentState === 'absent' || a.kind === 'android') continue
    let h = 7
    for (const c of a.id) h = (h * 33 + c.charCodeAt(0)) >>> 0
    if (h % 3 === 0) continue
    const r: UpdatesWaitingRow = { assetId: a.id, hostname: a.hostname, updates: h % 4 === 0 ? 0 : 1 + (h % 11), security: h % 5 === 0 ? 1 + (h % 3) : 0, rebootRequired: h % 7 === 0 }
    if (h % 4 === 1) { r.agent = 'behind'; r.agentVersion = '1.38.2' }
    if (/ubuntu 22/i.test(a.os)) r.releaseNext = 'Ubuntu 24.04 LTS'
    else if (/rhel 9|red hat.* 9/i.test(a.os)) r.releaseNext = 'RHEL 10'
    if (needsUpdates(r)) rows.push(r)
  }
  const count = (f: (r: UpdatesWaitingRow) => boolean) => rows.filter(f).length
  return {
    latest, assets: rows,
    securityAssets: count((r) => r.security > 0), securityFixes: rows.reduce((n, r) => n + r.security, 0),
    updateAssets: count((r) => r.updates > 0), rebootAssets: count((r) => r.rebootRequired),
    agentsBehind: count((r) => r.agent === 'behind'), releaseAssets: count((r) => !!r.releaseNext),
  }
}
