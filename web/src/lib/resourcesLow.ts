import { get } from '~/lib/live'
import type { ResourceWarning } from '~/types/domain'

/*
 * Which assets are running low, from the control plane's one rule
 * (internal/control/resources.go): the assets list's Running low filter and
 * its badges. Only usage reported in the last few minutes counts, so an
 * agent that went quiet neither raises nor keeps a badge. Kept out of
 * live.ts, which every page loads.
 */

/** How recent usage must be to warn, as the control plane counts it (resourcesFresh). */
export const LOW_FRESH_MS = 5 * 60 * 1000

export interface ResourcesLowRow {
  assetId: string
  hostname: string
  usageAt: string
  /** Critical first. */
  warnings: ResourceWarning[]
}

export interface ResourcesLow {
  assets: ResourcesLowRow[]
  /** How recent usage had to be to count. */
  freshSeconds: number
}

export const resourcesLowQuery = () => ({
  queryKey: ['resources-low'] as const,
  queryFn: (): Promise<ResourcesLow> => get('/api/v1/resources-low'),
  staleTime: 60_000,
  refetchInterval: 60_000,
})

/** The tone a warning is shown in: rose when critical, amber when low. */
export function warningTone(w: ResourceWarning | undefined): 'rose' | 'amber' | undefined {
  return w ? (w.level === 'critical' ? 'rose' : 'amber') : undefined
}

/** One line saying what is running low and how much is left. */
export function warningText(w: ResourceWarning): string {
  switch (w.resource) {
    case 'memory': return `Memory: ${bytes(w.left)} of ${bytes(w.total)} left (${w.percent}%)`
    case 'disk': return `Disk ${w.mount ?? ''} is ${w.percent}% full, ${bytes(w.left)} left`
    case 'swap': return `Swap: ${w.percent}% used, ${bytes(w.left)} left`
    case 'cpu': return `Processor: ${w.percent}% busy over the last minute`
  }
}

/** A badge's few words: "Memory 4% left", "Disk / 96% full". */
export function warningBadge(w: ResourceWarning): string {
  switch (w.resource) {
    case 'memory': return `Memory ${w.percent}% left`
    case 'disk': return `Disk ${w.mount ?? ''} ${w.percent}% full`
    case 'swap': return `Swap ${w.percent}% used`
    case 'cpu': return `CPU ${w.percent}%`
  }
}

const GiB = 1024 ** 3

/** A size with its unit. */
export function bytes(n: number): string {
  const v = n / GiB
  if (v >= 1024) return `${trim((v / 1024).toFixed(1))} TB`
  if (v >= 1) return `${v >= 100 ? Math.round(v) : trim(v.toFixed(1))} GB`
  return `${Math.round(n / 1024 ** 2)} MB`
}

export function trim(s: string): string {
  return s.endsWith('.0') ? s.slice(0, -2) : s
}
