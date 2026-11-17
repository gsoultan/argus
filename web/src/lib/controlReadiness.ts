import { useMutation, useQueryClient } from '@tanstack/react-query'
import { get, send } from '~/lib/live'

/*
 * Settings › Control plane › If Argus goes down. Kept out of live.ts and
 * queries.ts, which every route loads, so only the settings page pays for it.
 */

/** A control server, as its own row in the database says. */
export interface ControlInstance {
  host: string
  version: string
  startedAt: string
  seenAt: string
  running: boolean
  checkAt?: string
  checkOk?: boolean
  checkDetail?: string
}

/** Who confirmed something Argus cannot see, and until when it holds. */
export interface ReadinessConfirmation {
  by: string
  at: string
  until: string
  lapsed: boolean
}

export type ReadinessItem = 'backups_off_host' | 'key_off_host' | 'provider_consoles'

/** GET /api/v1/platform/readiness. */
export interface ReadinessView {
  outsideCheck: {
    configured: boolean
    host?: string
    every?: string
    lastAt?: string
    ok?: boolean
    detail?: string
  }
  servers: ControlInstance[]
  running: number
  backups: { installed: boolean; timerOn: boolean; lastAt?: string; lastOk: boolean; lastError?: string }
  directSsh: { refusing: number; total: number }
  confirmations: Record<ReadinessItem, ReadinessConfirmation | null>
  lastOutage?: { startedAt: string; endedAt: string; foundBy: string }
  confirmDays: number
}

/** Where the runbook lives: outside Argus, so it can be read while Argus is down. */
export const RUNBOOK_URL = 'https://github.com/gsoultan/argus/blob/main/docs/argus-down.md'

export const readinessKey = ['platform', 'readiness'] as const

export async function readiness(): Promise<ReadinessView> {
  const v = await get<ReadinessView>('/api/v1/platform/readiness')
  // A control plane, or a proxy in front of one, that answers with something
  // else must not take the settings page down with it.
  if (!v || typeof v !== 'object' || !v.outsideCheck || !Array.isArray(v.servers) || !v.backups ||
      !v.directSsh || !v.confirmations) {
    throw new Error('argus-control did not answer with a readiness report')
  }
  return v
}

export async function confirmReadiness(item: ReadinessItem): Promise<ReadinessConfirmation> {
  return send<ReadinessConfirmation>('POST', `/api/v1/platform/readiness/${item}/confirm`)
}

export async function testOutsideCheck(): Promise<{ ok: boolean; detail: string }> {
  return send<{ ok: boolean; detail: string }>('POST', '/api/v1/platform/outside-check/test')
}

/** Confirms one thing Argus cannot see for itself. */
export function useConfirmReadiness() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (item: ReadinessItem) => confirmReadiness(item),
    onSettled: () => void qc.invalidateQueries({ queryKey: readinessKey }),
  })
}

/** Calls the outside check-in now. */
export function useTestOutsideCheck() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => testOutsideCheck(),
    onSettled: () => void qc.invalidateQueries({ queryKey: readinessKey }),
  })
}

export type Tone = 'ok' | 'warn' | 'bad' | 'ask'

/** One line on the page: what it says, and how it stands. */
export interface ReadinessLine {
  id: string
  tone: Tone
  title: string
  detail: string
}

const STALE_CHECK_MS = 10 * 60_000

/**
 * The lines Argus works out for itself, worst first within the page's
 * order. Pure, so the rule for each tone is tested without a page.
 */
export function readinessChecks(v: ReadinessView, now = Date.now()): ReadinessLine[] {
  const out: ReadinessLine[] = []
  const oc = v.outsideCheck
  if (!oc.configured) {
    out.push({ id: 'outside', tone: 'bad', title: 'Nothing outside Argus will notice it stop',
      detail: 'Set a check-in address under Alerts. Argus calls it every minute while it is up; when the calls stop, that service alerts you, even when this server is off.' })
  } else if (!oc.lastAt || now - Date.parse(oc.lastAt) > STALE_CHECK_MS) {
    out.push({ id: 'outside', tone: 'warn', title: `A check-in to ${oc.host || 'the outside service'} is set but has not run lately`,
      detail: 'Press Send a test under Alerts to see why.' })
  } else if (oc.ok === false) {
    out.push({ id: 'outside', tone: 'bad', title: 'The last outside check-in failed',
      detail: oc.detail || 'The service did not accept it.' })
  } else {
    out.push({ id: 'outside', tone: 'ok', title: 'Something outside Argus will notice it stop',
      detail: `Argus tells ${oc.host || 'the check-in service'} it is up every ${oc.every === '5m' ? '5 minutes' : 'minute'}. If it stops, that service alerts you, even when this server is off.` })
  }

  const running = v.servers.filter((s) => s.running)
  if (running.length >= 2) {
    out.push({ id: 'servers', tone: 'ok', title: `${running.length} control servers are running`,
      detail: `${running.map((s) => s.host).join(', ')}. If one stops, the others keep the console, approvals and sessions going.` })
  } else {
    out.push({ id: 'servers', tone: 'warn', title: 'Only one control server is running',
      detail: `${running[0]?.host ?? 'This server'} runs alone. If it stops, the console, approvals and new sessions stop with it. A second server behind a load balancer, on the same database, keeps them going.` })
  }

  const b = v.backups
  if (!b.installed) {
    out.push({ id: 'backups', tone: 'bad', title: 'Backups are not set up on this server',
      detail: 'argus-backup.service is not installed. Without a backup, a lost server is a lost audit trail.' })
  } else if (!b.lastAt) {
    out.push({ id: 'backups', tone: 'bad', title: 'No backup has run yet', detail: 'Run one from Backups.' })
  } else if (!b.lastOk) {
    out.push({ id: 'backups', tone: 'bad', title: 'The last backup failed', detail: b.lastError || 'See Backups.' })
  } else if (now - Date.parse(b.lastAt) > 36 * 3600_000) {
    out.push({ id: 'backups', tone: 'warn', title: 'The last backup is more than a day old',
      detail: b.timerOn ? 'The nightly timer is on, but it has not run. See Backups.' : 'The nightly timer is off. See Backups.' })
  } else {
    out.push({ id: 'backups', tone: 'ok', title: 'A backup ran in the last day', detail: 'The database, the audit chain and the recordings.' })
  }

  const d = v.directSsh
  if (d.total > 0 && d.refusing > 0) {
    out.push({ id: 'ssh', tone: 'warn', title: `${d.refusing} of ${d.total} servers refuse plain SSH while Argus is down`,
      detail: 'They are set to refuse direct logins or ask for approval, which needs Argus to answer. During an outage the way in is the provider\'s console (Nutanix Prism, the cloud\'s serial console) with a local password.' })
  } else if (d.total > 0) {
    out.push({ id: 'ssh', tone: 'ok', title: 'Every server lets plain SSH in, recorded',
      detail: 'While Argus is down, people can still log in, and the agent records them on the server.' })
  }
  return out
}

/** What each confirmation says, in page order. */
export const CONFIRM_ITEMS: { id: ReadinessItem; title: string; detail: string }[] = [
  { id: 'backups_off_host', title: 'Backups are copied off this server',
    detail: 'A backup on the server it protects covers mistakes, not the loss of the server. Copy /var/backups/argus somewhere else.' },
  { id: 'key_off_host', title: 'The signing key is kept somewhere this server is not',
    detail: 'Without it, the database backups Argus takes cannot be opened. It is signing_secret under /var/lib/argus-control/secrets/.' },
  { id: 'provider_consoles', title: 'Someone knows where each server\'s provider console is',
    detail: 'And at least one local account on each server has a password stored safely, because keys do not work on a console.' },
]

/** How a confirmation stands. */
export function confirmTone(c: ReadinessConfirmation | null): Tone {
  if (!c) return 'ask'
  return c.lapsed ? 'warn' : 'ok'
}
