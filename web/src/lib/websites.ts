import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { get, send } from '~/lib/live'

/*
 * Websites: is a site up, and how much of the time. The control plane does
 * the checking and decides what counts as down (internal/control/websites.go);
 * this only reads and edits. Kept out of live.ts and queries.ts, which every
 * route loads, so only the Websites page pays for it.
 *
 * There is no fixture: a page that says whether real sites are up must not
 * show invented ones, so without a control plane the page says so instead.
 */

/** pending: not checked yet. unknown: Argus could not check, which is not down. */
export type WebsiteState = 'pending' | 'up' | 'down' | 'unknown'
/** partial: Argus could not reach it, a chosen server could, so it is up. */
export type RoundOutcome = 'up' | 'partial' | 'pending' | 'down' | 'unknown'

export interface WebsiteUptime {
  day: number | null
  week: number | null
  month: number | null
}

export interface WebsiteOutage {
  id: number
  checkId: string
  startedAt: string
  endedAt?: string
  cause: string
  confirmed: string
}

export interface WebsiteCheck {
  id: string
  name: string
  url: string
  expectStatus: number
  keyword: string
  everySeconds: number
  timeoutSeconds: number
  confirmFrom: string[]
  confirmHosts: string[]
  paused: boolean
  /** default follows the Websites setting; off sends none; chats sends to telegramChats. */
  telegram: TelegramChoice
  /** With chats: '' the default bot, 'platform', or one of the organization's bots. */
  telegramBot: string
  telegramChats: string[]
  createdBy: string
  createdAt: string
  state: WebsiteState
  stateSince?: string
  failingRounds: number
  lastAt?: string
  lastMs?: number
  lastStatus?: number
  lastDetail: string
  /** The last round's outcome; '' before the first. */
  lastOutcome: RoundOutcome | ''
  certNotAfter?: string
  uptime: WebsiteUptime
  openOutage?: WebsiteOutage
}

export interface WebsiteRound {
  at: string
  secs: number
  outcome: RoundOutcome
  ms?: number
  status?: number
  detail: string
}

export interface WebsiteDetail {
  check: WebsiteCheck
  rounds: WebsiteRound[]
  outages: WebsiteOutage[]
}

export interface WebsiteInput {
  name: string
  url: string
  expectStatus: number
  keyword: string
  everySeconds: number
  timeoutSeconds: number
  confirmFrom: string[]
  paused: boolean
  telegram: TelegramChoice
  /** With chats: '' the default bot, 'platform', or one of the organization's bots. */
  telegramBot: string
  telegramChats: string[]
}

export type TelegramChoice = 'default' | 'off' | 'chats'

/** One fetch, as the Test button gets it back. */
export interface SiteResult {
  ok: boolean
  status?: number
  fail?: string
  detail?: string
  ms: number
  certNotAfter?: string
}

const key = ['websites'] as const

export const websitesQuery = () =>
  queryOptions({ queryKey: key, queryFn: () => get<WebsiteCheck[]>('/api/v1/websites'), refetchInterval: 30_000 })

export const websiteQuery = (id: string) =>
  queryOptions({
    queryKey: [...key, id] as const,
    queryFn: () => get<WebsiteDetail>(`/api/v1/websites/${encodeURIComponent(id)}`),
    refetchInterval: 30_000,
  })

function useWebsiteMutation<A, R>(fn: (a: A) => Promise<R>) {
  const qc = useQueryClient()
  return useMutation({ mutationFn: fn, onSettled: () => void qc.invalidateQueries({ queryKey: key }) })
}

export const useAddWebsite = () =>
  useWebsiteMutation((in_: WebsiteInput) => send<WebsiteCheck>('POST', '/api/v1/websites', in_))

export const useChangeWebsite = () =>
  useWebsiteMutation(({ id, input }: { id: string; input: WebsiteInput }) =>
    send<WebsiteCheck>('PUT', `/api/v1/websites/${encodeURIComponent(id)}`, input))

export const useRemoveWebsite = () =>
  useWebsiteMutation((id: string) => send<void>('DELETE', `/api/v1/websites/${encodeURIComponent(id)}`))

export const useCheckWebsiteNow = () =>
  useWebsiteMutation((id: string) => send<void>('POST', `/api/v1/websites/${encodeURIComponent(id)}/check`))

export const testWebsiteAlert = (id: string) =>
  send<{ bot: string; chats: string[] }>('POST', `/api/v1/websites/${encodeURIComponent(id)}/test-alert`)

export const testWebsite = (in_: WebsiteInput) => send<SiteResult>('POST', '/api/v1/websites/test', in_)

/** "99.95%", "100%", or "—" when nothing was counted. Never rounds a gap up to 100. */
export function formatUptime(v: number | null | undefined): string {
  if (v == null) return '—'
  if (v >= 100) return '100%'
  const digits = v >= 99 ? 2 : 1
  const f = 10 ** digits
  return `${(Math.floor(v * f) / f).toFixed(digits)}%`
}

/** Minutes between two times, at least 1. */
export function outageMinutes(o: WebsiteOutage, now = Date.now()): number {
  const end = o.endedAt ? Date.parse(o.endedAt) : now
  return Math.max(1, Math.round((end - Date.parse(o.startedAt)) / 60_000))
}

/** "4 min", "2 h 5 min", "3 d 4 h". */
export function formatDuration(mins: number): string {
  if (mins < 60) return `${mins} min`
  const h = Math.floor(mins / 60)
  if (h < 24) return mins % 60 ? `${h} h ${mins % 60} min` : `${h} h`
  const d = Math.floor(h / 24)
  return h % 24 ? `${d} d ${h % 24} h` : `${d} d`
}

/** The state in a word, and its colour. */
export function stateLabel(c: Pick<WebsiteCheck, 'state' | 'paused' | 'failingRounds'>): { label: string; color: string } {
  if (c.paused) return { label: 'Paused', color: 'slate' }
  switch (c.state) {
    case 'down': return { label: 'Down', color: 'rose' }
    case 'unknown': return { label: 'Cannot check', color: 'slate' }
    case 'pending': return c.failingRounds > 0 ? { label: 'Checking again', color: 'amber' } : { label: 'Not checked yet', color: 'slate' }
    default: return c.failingRounds > 0 ? { label: 'Checking again', color: 'amber' } : { label: 'Up', color: 'teal' }
  }
}

/** "just now", "5 min ago", "3 h ago", "2 d ago". */
export function ago(iso: string | undefined, now = Date.now()): string {
  if (!iso) return 'never'
  const mins = Math.round((now - Date.parse(iso)) / 60_000)
  if (mins < 1) return 'just now'
  return `${formatDuration(mins)} ago`
}

/** Days until a certificate ends, or null. */
export function certDaysLeft(iso: string | undefined, now = Date.now()): number | null {
  if (!iso) return null
  return Math.floor((Date.parse(iso) - now) / 86_400_000)
}

const WORST: RoundOutcome[] = ['down', 'pending', 'unknown', 'partial', 'up']

/** A day of rounds in n even slices of time: each slice's worst outcome and its average answer when up. */
export function sliceRounds(rounds: WebsiteRound[], n: number, now = Date.now()): ({ from: number; outcome: RoundOutcome; ms: number | null; count: number } | null)[] {
  const start = now - 86_400_000
  const width = 86_400_000 / n
  const out: ({ from: number; outcome: RoundOutcome; ms: number | null; count: number } | null)[] = Array(n).fill(null)
  const sums: number[] = Array(n).fill(0)
  const ups: number[] = Array(n).fill(0)
  for (const r of rounds) {
    const i = Math.floor((Date.parse(r.at) - start) / width)
    if (i < 0 || i >= n) continue
    const cur = out[i] ?? { from: start + i * width, outcome: 'up' as RoundOutcome, ms: null, count: 0 }
    cur.count++
    if (WORST.indexOf(r.outcome) < WORST.indexOf(cur.outcome)) cur.outcome = r.outcome
    if ((r.outcome === 'up' || r.outcome === 'partial') && r.ms != null) { sums[i] = (sums[i] ?? 0) + r.ms; ups[i] = (ups[i] ?? 0) + 1 }
    out[i] = cur
  }
  out.forEach((s, i) => { const n = ups[i] ?? 0; if (s && n) s.ms = Math.round((sums[i] ?? 0) / n) })
  return out
}
