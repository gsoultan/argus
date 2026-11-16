import { get, live } from '~/lib/live'
import { api } from '~/lib/api'
import type { Asset, ResourceWarning } from '~/types/domain'

/*
 * The usage row on each Assets card: processor, memory and the fullest disk
 * from the agent's last report, with the running-low warnings
 * (internal/control/assets_usage.go). Every role reads it, and the control
 * plane answers each with the assets that person sees on Assets: the whole
 * fleet for admins, owners and viewers, the servers given to them for
 * everyone else. Like the running-low list, only usage reported in the last
 * few minutes counts, so a quiet agent shows no figures rather than old ones.
 */

export interface AssetUsageRow {
  assetId: string
  usageAt: string
  /** How busy the processor was over the last minute, in percent. */
  cpu: number | null
  /** How much memory is in use, in percent. */
  memory: number | null
  /** The fullest disk, counted like df. */
  disk: { mount: string; percent: number } | null
  /** Critical first; empty when nothing is running low. */
  warnings: ResourceWarning[]
}

export interface AssetsUsage {
  assets: AssetUsageRow[]
  freshSeconds: number
}

export const assetsUsageQuery = () => ({
  queryKey: ['assets-usage'] as const,
  queryFn: (): Promise<AssetsUsage> =>
    live.orFallback(() => get<AssetsUsage>('/api/v1/assets-usage'), async () => fixtureUsage(await api.assets({}))),
  staleTime: 60_000,
  refetchInterval: 60_000,
})

/** The colour a figure is drawn in, at the thresholds the warnings use. */
export function usageTone(percent: number): 'rose' | 'amber' | 'azure' {
  return percent >= 95 ? 'rose' : percent >= 90 ? 'amber' : 'azure'
}

/** Made-up figures for the fixture, steady per asset so a reload agrees. */
function fixtureUsage(assets: Asset[]): AssetsUsage {
  const GB = 1024 ** 3
  const at = new Date().toISOString()
  const rows: AssetUsageRow[] = []
  for (const a of assets) {
    if (a.kind === 'android' || a.agentState === 'absent' || a.protocol === 'rdp') continue
    let h = 0
    for (const c of a.id) h = (h * 31 + c.charCodeAt(0)) >>> 0
    const cpu = 4 + (h % 58)
    const memory = 28 + ((h >>> 6) % 52)
    const fullest = (h >>> 12) % 7 === 0 ? 91 : 18 + ((h >>> 12) % 60)
    const mount = fullest >= 90 ? '/var/lib/postgresql' : '/'
    const warnings: ResourceWarning[] = fullest >= 90
      ? [{ resource: 'disk', level: fullest >= 95 ? 'critical' : 'low', mount, percent: fullest, left: 45 * GB, total: 500 * GB }]
      : []
    rows.push({ assetId: a.id, usageAt: at, cpu, memory, disk: { mount, percent: fullest }, warnings })
  }
  return { assets: rows, freshSeconds: 300 }
}
