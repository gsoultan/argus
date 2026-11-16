import { queryOptions } from '@tanstack/react-query'
import { get, isConfigured, post } from '~/lib/live'
import type { CloudEnvironment, CloudItem } from '~/lib/cloud'

/*
 * Cloud › Reservations: Reserved Instances, Savings Plans and Capacity
 * Reservations, read with each account's inventory. "Use" is Argus's
 * estimate against the instances running at the last read, worked out on
 * the control plane the way AWS applies them.
 */

export interface ReservationsAccount {
  id: string
  name: string
  environment: CloudEnvironment
  /** False before the first reservations read. */
  read: boolean
  reservedInstances: boolean
  capacityReservations: boolean
  savingsPlans: boolean
  /** The read role predates Savings Plans and AWS refused the read. */
  savingsPlansNeedRole: boolean
}

export interface RIUse {
  units: number
  used: number
  /** 0 to 1. */
  use: number
  monthly: number
  flexible: boolean
  covers?: string[]
}

export interface SPUse {
  commitment: number
  /** List price an hour of running instances it could apply to. */
  eligible: number
  alsoOther: boolean
}

export interface Coverage {
  accountId: string
  accountName: string
  region: string
  instances: number
  priced: number
  /** List price an hour of what runs. */
  hourly: number
  byRi: number
  rest: number
}

export type ReservationItem = Omit<CloudItem, 'resource'> & { resource: Omit<CloudItem['resource'], 'kind'> & { kind: string } }

export interface ReservationsView {
  accounts: ReservationsAccount[]
  reservedInstances: (ReservationItem & { use: RIUse })[]
  savingsPlans: (ReservationItem & { use: SPUse })[]
  capacityReservations: ReservationItem[]
  coverage: Coverage[]
  /** Paid for and not used a month, by accountId/resourceId; the same sums Risks & waste shows. */
  unused: Record<string, number>
  unusedMonthly: number
  checkedAt: string
}

export const cloudReservationsQuery = () =>
  queryOptions({
    queryKey: ['cloud', 'reservations'],
    queryFn: () => get<ReservationsView>('/api/v1/cloud/reservations'),
    enabled: isConfigured(),
    refetchInterval: 5 * 60_000,
  })

export const HOURS_A_MONTH = 730

/** Days until an end date, rounded up; null without one. */
export function daysUntil(end: string | undefined, now = Date.now()): number | null {
  if (!end) return null
  const t = Date.parse(end)
  if (Number.isNaN(t)) return null
  return Math.ceil((t - now) / 86_400_000)
}

const PAYMENT: Record<string, string> = {
  'no upfront': 'no upfront', 'partial upfront': 'partial upfront', 'all upfront': 'all upfront',
  noupfront: 'no upfront', partialupfront: 'partial upfront', allupfront: 'all upfront',
}

/** "1 yr · no upfront" from a term in months and AWS's payment words. */
export function termWords(months: string | undefined, payment: string | undefined): string {
  const t = months === '36' ? '3 yr' : months === '12' ? '1 yr' : ''
  const raw = (payment ?? '').toLowerCase()
  const p = PAYMENT[raw] ?? PAYMENT[raw.replace(/\s+/g, '')] ?? raw
  return [t, p].filter(Boolean).join(' · ') || '—'
}

export type HeldKind = 'ri' | 'sp' | 'cr'

/** One thing an account holds, as the table shows it. */
export interface Held {
  kind: HeldKind
  key: string
  item: ReservationItem
  covers: string
  where: string
  term: string
  ends?: string
  /** 0 to 1, or null when Argus cannot tell. */
  use: number | null
  useNote: string
  /** Paid for and not used, a month. */
  unused: number
}

export function held(v: ReservationsView): Held[] {
  const out: Held[] = []
  const un = (it: ReservationItem) => v.unused[`${it.accountId}/${it.resource.id}`] ?? 0
  for (const it of v.reservedInstances) {
    const d = it.resource.details ?? {}
    out.push({
      kind: 'ri', key: it.accountId + it.resource.id, item: it, covers: `${d.count ?? '?'} × ${it.resource.size ?? ''}`,
      where: d.scope === 'zone' ? it.resource.zone ?? it.resource.region : it.resource.region,
      term: termWords(d.termMonths, d.payment), ends: d.end, use: it.use.use, unused: un(it),
      useNote: it.use.covers?.length ? `On ${it.use.covers.join(', ')}` : 'No running instance matches it',
    })
  }
  for (const it of v.savingsPlans) {
    const d = it.resource.details ?? {}
    const what = d.type === 'EC2Instance' ? `${d.family ?? ''} instances` : d.type === 'Compute' ? 'compute' : (d.type ?? '').toLowerCase()
    const eligible = it.use.eligible
    out.push({
      kind: 'sp', key: it.accountId + it.resource.id, item: it, covers: `$${Number(d.commitment ?? 0).toFixed(2)}/hr ${what}`.trim(),
      where: d.planRegion || 'all regions', term: termWords(d.termMonths, d.payment), ends: d.end, unused: un(it), use: null,
      useNote: eligible > 0
        ? `$${eligible.toFixed(2)}/hr of instances it could cover, at list price${it.use.alsoOther ? '; it also covers Fargate and Lambda' : ''}`
        : it.use.alsoOther ? 'No instances to cover; it may be covering Fargate or Lambda' : 'Nothing running in its family and region',
    })
  }
  for (const it of v.capacityReservations) {
    const d = it.resource.details ?? {}
    const total = Number(d.total ?? 0)
    const free = Number(d.available ?? 0)
    out.push({
      kind: 'cr', key: it.accountId + it.resource.id, item: it, covers: `${total} × ${it.resource.size ?? ''}`,
      where: it.resource.zone ?? it.resource.region, term: d.match === 'targeted' ? 'Targeted launches only' : 'Any matching instance',
      ends: d.end, use: total > 0 ? (total - free) / total : null, unused: un(it),
      useNote: free > 0 ? `${free} empty, billed as if running` : 'Full',
    })
  }
  return out
}

/* ── Buying a Reserved Instance or Savings Plan ─────────────────────────── */

export type Payment = 'No Upfront' | 'Partial Upfront' | 'All Upfront'

/** What to buy, as the control plane takes it. */
export interface OfferQuery {
  kind: 'ri' | 'sp'
  region: string
  termMonths: 12 | 36
  payment: Payment
  size?: string
  count?: number
  planType?: 'Compute' | 'EC2Instance'
  family?: string
  /** Dollars an hour. */
  commitment?: number
}

export interface Offer { id: string; upfront: number; hourly: number; total: number; monthly: number }

export interface Quote {
  offer: Offer
  /** The total as it is typed to confirm. */
  totalText: string
  /** List price of the same hours over the term; 0 for a Savings Plan. */
  onDemand: number
  saves: number
  /** The month it costs less than on demand would have; 0 when it never does. */
  paysOffMonth: number
  covers: string[]
  alwaysApproval: boolean
}

export const cloudQuoteQuery = (accountId: string | null, q: OfferQuery | null) =>
  queryOptions({
    queryKey: ['cloud', 'quote', accountId, q],
    queryFn: () => post<Quote>('/api/v1/cloud/reservations/quote', { accountId, query: q }),
    enabled: isConfigured() && Boolean(accountId && q),
    staleTime: 0,
    gcTime: 0,
    retry: false,
  })

/** Whether a typed total is the quoted one, as the control plane checks it. */
export function sameTotal(typed: string, total: number): boolean {
  const f = Number(typed.replace(/[$,\s]|usd/gi, ''))
  if (!typed.trim() || Number.isNaN(f) || f < 0) return false
  return total >= 100 ? Math.round(f) === Math.round(total) : Math.abs(f - total) < 0.005
}

/** The family of a size: m6i of m6i.large. */
export const familyOf = (size: string) => size.split('.')[0] ?? ''
