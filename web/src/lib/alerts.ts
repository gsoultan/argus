import { infiniteQueryOptions, queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  alertPrefs, alertStreamURL, consoleAlerts, isConfigured, markAlertsRead, saveAlertPrefs,
  type AlertKind, type AlertPage, type AlertPrefs, type ConsoleAlert,
} from '~/lib/live'

/**
 * The bell in the console header: its queries, and the one live stream that
 * keeps them current.
 *
 * The stream says that something changed; the list and the unread count are
 * always fetched. So an event the stream missed, across a reconnect or from
 * another replica, costs a pop-up and never a wrong badge.
 */

export type { AlertKind, AlertPage, AlertPrefs, ConsoleAlert }

export const ALERT_KINDS: { kind: AlertKind; label: string; description: string; who: string }[] = [
  { kind: 'approval', label: 'Approval requests',
    description: 'Someone asks for access you can decide, or a login waits for one. Also when your own request is decided.',
    who: 'Approvers, admins' },
  { kind: 'signin', label: 'Console sign-ins', description: 'Someone signs in to Argus.', who: 'Admins, auditors' },
  { kind: 'session', label: 'Sessions opened',
    description: 'A terminal, desktop or database session starts, through Argus or around it.', who: 'Admins, auditors' },
  { kind: 'security', label: 'Security alerts',
    description: 'A recovery code used, an authenticator reset, sign-ins refused, "this wasn\'t me", backups missed.',
    who: 'Admins, auditors' },
  { kind: 'server', label: 'Servers', description: 'A server joins Argus, or its agent is uninstalled.', who: 'Admins, auditors' },
]

/* ── Fixture ─────────────────────────────────────────────────────────────── */

// Without a control plane the bell shows these, under the same "sample data"
// the rest of the console is labelled with. Module scope, so marking one read
// lasts until a reload, like the rest of the fixture.
const ago = (min: number) => new Date(Date.now() - min * 60_000).toISOString()
let fixtureRead = new Set<number>()
const FIXTURE: Omit<ConsoleAlert, 'read'>[] = [
  { seq: 6, at: ago(2), kind: 'approval', severity: 'warning', title: 'rina@northwind.id asks for root on db-02',
    detail: 'Rotate the replication slot, ticket OPS-412.', link: '/approvals' },
  { seq: 5, at: ago(9), kind: 'signin', severity: 'warning', title: 'dewi@northwind.id signed in from a new device',
    detail: 'Chrome 131 on macOS · 182.253.14.7 · authenticator app', link: '/audit' },
  { seq: 4, at: ago(24), kind: 'session', severity: 'warning', title: 'Direct login on web-01 as deploy',
    detail: 'Not through Argus. The agent recorded it.', link: '/activity' },
  { seq: 3, at: ago(64), kind: 'session', severity: 'info', title: 'budi@northwind.id opened SSH on app-03 as deploy',
    detail: 'From 10.20.4.17.', link: '/activity' },
  { seq: 2, at: ago(130), kind: 'approval', severity: 'info', title: 'Your request for deploy on web-01 was approved',
    detail: 'Approved by rizal@northwind.id.', link: '/approvals' },
  { seq: 1, at: ago(190), kind: 'signin', severity: 'info', title: 'rizal@northwind.id signed in',
    detail: 'Firefox 132 on Windows · 10.0.0.12 · authenticator app', link: '/audit' },
]
let fixturePrefs: AlertPrefs = { muted: [], newDevicesOnly: false, popup: true, sound: false }

function fixturePage(unread: boolean): AlertPage {
  const visible = FIXTURE.filter((a) => a.kind === 'security' || !fixturePrefs.muted.includes(a.kind))
    .filter((a) => !(a.kind === 'signin' && fixturePrefs.newDevicesOnly && a.severity === 'info'))
    .map((a) => ({ ...a, read: fixtureRead.has(a.seq) }))
  return {
    alerts: unread ? visible.filter((a) => !a.read) : visible,
    unread: visible.filter((a) => !a.read).length,
    more: false,
  }
}

/* ── Queries ─────────────────────────────────────────────────────────────── */

export const alertKeys = {
  all: ['alerts'] as const,
  list: (unread: boolean) => ['alerts', 'list', unread] as const,
  prefs: ['alerts', 'prefs'] as const,
}

/** The unread alerts: the badge, and the first tab. */
export const unreadAlertsQuery = () =>
  queryOptions({
    queryKey: alertKeys.list(true),
    queryFn: () => (isConfigured() ? consoleAlerts(true) : Promise.resolve(fixturePage(true))),
    // The stream is what keeps this current. The interval is the net under
    // it, for a proxy that holds events back or a stream that never opened.
    refetchInterval: 120_000,
  })

/** Every alert, newest first, a page at a time. */
export const allAlertsQuery = () =>
  infiniteQueryOptions({
    queryKey: alertKeys.list(false),
    queryFn: ({ pageParam }) =>
      isConfigured() ? consoleAlerts(false, pageParam || undefined) : Promise.resolve(fixturePage(false)),
    initialPageParam: 0,
    getNextPageParam: (last: AlertPage) => (last.more ? last.alerts[last.alerts.length - 1]?.seq : undefined),
  })

export const alertPrefsQuery = () =>
  queryOptions({
    queryKey: alertKeys.prefs,
    queryFn: () => (isConfigured() ? alertPrefs() : Promise.resolve(fixturePrefs)),
    staleTime: 5 * 60_000,
  })

export function useMarkAlertsRead() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: async (input: { seqs?: number[]; upTo?: number }) => {
      if (isConfigured()) return markAlertsRead(input)
      if (input.upTo) fixtureRead = new Set(FIXTURE.filter((a) => a.seq <= input.upTo!).map((a) => a.seq))
      for (const s of input.seqs ?? []) fixtureRead.add(s)
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ['alerts', 'list'] }),
  })
}

export function useSaveAlertPrefs() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: async (p: AlertPrefs) => {
      if (isConfigured()) return saveAlertPrefs(p)
      fixturePrefs = { ...p, muted: p.muted.filter((k) => k !== 'security') }
      return fixturePrefs
    },
    onSuccess: (p) => {
      qc.setQueryData(alertKeys.prefs, p)
      void qc.invalidateQueries({ queryKey: ['alerts', 'list'] })
    },
  })
}

/* ── The stream ──────────────────────────────────────────────────────────── */

export type StreamStatus = 'fixture' | 'connecting' | 'live' | 'reconnecting' | 'ended'

export type StreamEvent =
  | { type: 'alert'; alert: ConsoleAlert }
  | { type: 'ready' | 'read' }
  | { type: 'end' }

type Message = { event: StreamEvent } | { status: StreamStatus } | { hello: true }

/**
 * One stream for every tab of the console in this browser.
 *
 * A tab that holds the lock opens the EventSource and passes what it hears to
 * the others on a BroadcastChannel; when it closes, the lock passes to another
 * tab, which opens its own. Over HTTP/1.1 a stream per tab would take one of
 * the six connections a browser allows a host, so a few tabs would stall the
 * console. Where the browser has no Web Locks, each tab opens its own.
 */
class AlertStream {
  private listeners = new Set<(m: Message) => void>()
  private status: StreamStatus = 'connecting'
  private bc: BroadcastChannel | null = null
  private es: EventSource | null = null
  private release: (() => void) | null = null
  private retry: ReturnType<typeof setTimeout> | null = null
  private stopped = true

  subscribe(fn: (m: Message) => void): () => void {
    this.listeners.add(fn)
    fn({ status: this.status })
    if (this.listeners.size === 1) this.start()
    return () => {
      this.listeners.delete(fn)
      if (this.listeners.size === 0) this.stop()
    }
  }

  private emit(m: Message, share: boolean) {
    if ('hello' in m) {
      // A tab that just started asks how the stream stands; whoever holds it
      // answers.
      if (this.es) this.bc?.postMessage({ status: this.status })
      return
    }
    if ('status' in m) this.status = m.status
    for (const fn of this.listeners) fn(m)
    if (share) this.bc?.postMessage(m)
  }

  private start() {
    this.stopped = false
    if (!isConfigured()) {
      this.emit({ status: 'fixture' }, false)
      return
    }
    if (typeof BroadcastChannel !== 'undefined') {
      this.bc = new BroadcastChannel('argus-alerts')
      this.bc.onmessage = (e: MessageEvent<Message>) => this.emit(e.data, false)
      this.bc.postMessage({ hello: true })
    }
    const locks = (navigator as Navigator & { locks?: LockManager }).locks
    if (locks && this.bc) {
      void locks.request('argus-alerts-stream', () => new Promise<void>((resolve) => {
        if (this.stopped) return resolve()
        this.release = resolve
        this.open()
      }))
    } else {
      this.open()
    }
  }

  private open() {
    if (this.stopped) return
    const es = new EventSource(alertStreamURL(), { withCredentials: true })
    this.es = es
    es.onopen = () => this.emit({ status: 'live' }, true)
    es.onerror = () => {
      if (es.readyState === EventSource.CLOSED) {
        // Refused outright (signed out, too many tabs, restarting):
        // EventSource gives up, so try again later ourselves.
        es.close()
        this.emit({ status: 'reconnecting' }, true)
        this.retry = setTimeout(() => this.open(), 15_000)
      } else {
        this.emit({ status: 'reconnecting' }, true)
      }
    }
    const on = (name: string, fn: (data: unknown) => StreamEvent) =>
      es.addEventListener(name, (e) => {
        let data: unknown = null
        try { data = JSON.parse((e as MessageEvent<string>).data) } catch { /* empty event */ }
        this.emit({ event: fn(data) }, true)
      })
    on('ready', () => ({ type: 'ready' }))
    on('read', () => ({ type: 'read' }))
    on('alert', (d) => ({ type: 'alert', alert: d as ConsoleAlert }))
    on('end', () => {
      es.close()
      this.emit({ status: 'ended' }, true)
      return { type: 'end' }
    })
  }

  private stop() {
    this.stopped = true
    if (this.retry) clearTimeout(this.retry)
    this.es?.close()
    this.es = null
    this.release?.()
    this.release = null
    this.bc?.close()
    this.bc = null
    this.status = 'connecting'
  }
}

const stream = new AlertStream()

/** Listens to the shared stream until the returned function is called. */
export function subscribeAlertStream(
  onEvent: (e: StreamEvent) => void,
  onStatus: (s: StreamStatus) => void,
): () => void {
  return stream.subscribe((m) => {
    if ('status' in m) onStatus(m.status)
    else if ('event' in m) onEvent(m.event)
  })
}

/* ── Desktop notifications and sound ─────────────────────────────────────── */

const DESKTOP_KEY = 'argus.alerts.desktop'

/** Whether this browser was asked to show alerts as desktop notifications. */
export function desktopWanted(): boolean {
  try { return localStorage.getItem(DESKTOP_KEY) === '1' } catch { return false }
}

export function setDesktopWanted(on: boolean): void {
  try { localStorage.setItem(DESKTOP_KEY, on ? '1' : '0') } catch { /* private window */ }
}

export function desktopPermission(): NotificationPermission | 'unsupported' {
  return typeof Notification === 'undefined' ? 'unsupported' : Notification.permission
}

/**
 * Shows an alert as a desktop notification, from a tab nobody is looking at.
 * Tagged by seq, so the same alert heard in several tabs is shown once.
 */
export function notifyDesktop(a: ConsoleAlert, open: (link: string) => void): void {
  if (!desktopWanted() || desktopPermission() !== 'granted' || document.visibilityState === 'visible') return
  try {
    const n = new Notification(a.title, { body: a.detail, tag: `argus-alert-${a.seq}`, icon: '/icon.svg' })
    n.onclick = () => {
      window.focus()
      if (a.link) open(a.link)
      n.close()
    }
  } catch {
    // Some mobile browsers only notify from a service worker.
  }
}

let audio: AudioContext | null = null

/** A short two-note chime, for what someone has to act on. */
export function chime(): void {
  try {
    audio ??= new AudioContext()
    const t = audio.currentTime
    for (const [i, f] of [880, 1320].entries()) {
      const o = audio.createOscillator()
      const g = audio.createGain()
      o.frequency.value = f
      g.gain.setValueAtTime(0.0001, t + i * 0.12)
      g.gain.exponentialRampToValueAtTime(0.15, t + i * 0.12 + 0.02)
      g.gain.exponentialRampToValueAtTime(0.0001, t + i * 0.12 + 0.25)
      o.connect(g).connect(audio.destination)
      o.start(t + i * 0.12)
      o.stop(t + i * 0.12 + 0.3)
    }
  } catch {
    // No audio, or the browser wants a gesture first.
  }
}

/** Whether an alert is one someone has to act on, which earns the chime. */
export function urgent(a: ConsoleAlert): boolean {
  return a.kind === 'security' || (a.kind === 'approval' && a.severity !== 'info')
}
