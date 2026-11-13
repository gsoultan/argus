import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { get, isConfigured, send } from '~/lib/live'
import type { CloudAction, CloudEnvironment, CloudKind } from '~/lib/cloud'

/*
 * Costs, idle resources and budgets, kept out of lib/cloud so the pages that
 * read cloud inventory do not carry them.
 */

export interface DayCost { date: string; amount: number }
export interface ServiceCost { service: string; thisMonth: number; lastMonth: number }

/** One account's spend, as AWS billed it. */
export interface Costs {
  month: string
  currency: string
  monthToDate: number
  lastMonth: number
  lastMonthToDate: number
  forecast: number | null
  /** The part of forecast that is AWS's prediction, from today to the month's end; the rest was spent before today. */
  forecastRest?: number | null
  days: DayCost[]
  services: ServiceCost[]
}

export interface CloudCostRead {
  accountId: string
  accountName: string
  cloudAccountId: string
  environment: CloudEnvironment
  readAt: string | null
  costs: Costs | null
  error?: string
  errorAt?: string
}

/** A budget with where it stands this month. */
export interface CloudBudget {
  id: string
  /** '' is every account. */
  accountId: string
  accountName?: string
  name: string
  amount: number
  thresholds: number[]
  /** Whether a forecast over the amount alerts. */
  forecast: boolean
  createdBy: string
  createdAt: string
  /** This month's spend so far in the budget's accounts. */
  spent: number
  /** The month's expected end; null when an account has no forecast. */
  projected: number | null
  /** The budget's accounts with no read for this month. */
  unread: number
}

export interface CloudIdleItem {
  accountId: string
  accountName: string
  environment: CloudEnvironment
  kind: CloudKind
  id: string
  name: string
  region: string
  why: string
  monthly: number
  atMost?: boolean
  since?: string
  fix?: CloudAction
}

export interface CloudCostsView {
  accounts: CloudCostRead[]
  budgets: CloudBudget[]
  intervalHours: number
  askEveryMinutes: number
}

const costsKey = ['cloud', 'costs'] as const
const idleKey = ['cloud', 'idle'] as const

export const cloudCostsQuery = (opts: { enabled?: boolean } = {}) =>
  queryOptions({
    queryKey: costsKey,
    queryFn: () => get<CloudCostsView>('/api/v1/cloud/costs'),
    enabled: isConfigured() && opts.enabled !== false,
    refetchInterval: 5 * 60_000,
  })

export const cloudIdleQuery = (opts: { enabled?: boolean } = {}) =>
  queryOptions({
    queryKey: idleKey,
    queryFn: () => get<{ items: CloudIdleItem[]; monthly: number }>('/api/v1/cloud/idle'),
    enabled: isConfigured() && opts.enabled !== false,
    refetchInterval: 5 * 60_000,
  })

export function useReadCosts() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (accountId: string) => send<CloudCostRead>('POST', `/api/v1/cloud/costs/${encodeURIComponent(accountId)}/read`),
    onSettled: () => void qc.invalidateQueries({ queryKey: costsKey }),
  })
}

export interface BudgetIn { accountId: string; name: string; amount: number; thresholds: number[]; forecast: boolean }

export function useSaveBudget() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, ...b }: BudgetIn & { id?: string }) =>
      id ? send('PUT', `/api/v1/cloud/budgets/${encodeURIComponent(id)}`, b) : send('POST', '/api/v1/cloud/budgets', b),
    onSettled: () => void qc.invalidateQueries({ queryKey: costsKey }),
  })
}

export function useDeleteBudget() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (id: string) => send('DELETE', `/api/v1/cloud/budgets/${encodeURIComponent(id)}`),
    onSettled: () => void qc.invalidateQueries({ queryKey: costsKey }),
  })
}

/** Dollars with cents: $1,234.50. */
export function dollars(n: number): string {
  return `${n < 0 ? '-' : ''}$${Math.abs(n).toLocaleString('en-US', { minimumFractionDigits: 2, maximumFractionDigits: 2 })}`
}

/** The change from before to now, as a signed whole percentage; null when there is nothing to compare with. */
export function change(now: number, before: number): number | null {
  if (before <= 0) return null
  return Math.round(((now - before) / before) * 100)
}

/** Every account's reads summed for the month they share; reads for an older month are left out. */
export function totals(reads: CloudCostRead[]) {
  const month = reads.map((r) => r.costs?.month).filter(Boolean).sort().pop()
  const cur = reads.filter((r) => r.costs && r.costs.month === month).map((r) => r.costs!)
  const sum = (f: (c: Costs) => number) => Math.round(cur.reduce((s, c) => s + f(c), 0) * 100) / 100
  const days = new Map<string, number>()
  const services = new Map<string, ServiceCost>()
  for (const c of cur) {
    for (const d of c.days) days.set(d.date, (days.get(d.date) ?? 0) + d.amount)
    for (const s of c.services) {
      const x = services.get(s.service) ?? { service: s.service, thisMonth: 0, lastMonth: 0 }
      x.thisMonth += s.thisMonth
      x.lastMonth += s.lastMonth
      services.set(s.service, x)
    }
  }
  const forecastWhole = cur.length > 0 && cur.every((c) => c.forecast !== null)
  return {
    month,
    read: cur.length,
    monthToDate: sum((c) => c.monthToDate),
    lastMonth: sum((c) => c.lastMonth),
    lastMonthToDate: sum((c) => c.lastMonthToDate),
    forecast: forecastWhole ? sum((c) => c.forecast ?? 0) : null,
    forecastRest: forecastWhole && cur.every((c) => c.forecastRest != null) ? sum((c) => c.forecastRest ?? 0) : null,
    days: [...days.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([date, amount]) => ({ date, amount: Math.round(amount * 100) / 100 })),
    services: [...services.values()].sort((a, b) => b.thisMonth - a.thisMonth || a.service.localeCompare(b.service)),
  }
}

/** Days from today to the end of month ("2026-11"), today included, in UTC as AWS counts them. */
export function daysLeft(month: string, now = new Date()): number {
  const [y = 0, m = 0] = month.split('-').map(Number)
  const end = Date.UTC(y, m, 1)
  const today = Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate())
  return Math.max(0, Math.round((end - today) / 86_400_000))
}

/** Where a budget stands: the share spent, and whether spend or forecast is over. */
export function budgetState(b: CloudBudget): { pct: number; over: boolean; forecastOver: boolean; passed: number } {
  const pct = b.amount > 0 ? (b.spent / b.amount) * 100 : 0
  const passed = Math.max(0, ...b.thresholds.filter((t) => pct >= t))
  return { pct, over: b.spent >= b.amount, forecastOver: b.projected !== null && b.projected > b.amount, passed }
}

/** "80, 100" into [80, 100]: whole percentages 1 to 200, sorted, at most five; null when any part is not one. */
export function parseThresholds(s: string): number[] | null {
  const parts = s.split(/[\s,%]+/).filter(Boolean)
  if (parts.length === 0) return []
  const out: number[] = []
  for (const p of parts) {
    if (!/^\d+$/.test(p)) return null
    const n = Number(p)
    if (n < 1 || n > 200) return null
    if (!out.includes(n)) out.push(n)
  }
  return out.length > 5 ? null : out.sort((a, b) => a - b)
}
