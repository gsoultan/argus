import { alertStreamURL, isConfigured, streamHome, type ConsoleAlert } from '~/lib/live'
import type { StreamEvent, StreamStatus } from '~/lib/alerts'
import type { HomeFrame } from '~/types/domain'

// Loaded after the first paint: the bell's count needs none of this.

type Message =
  | { event: StreamEvent }
  | { status: StreamStatus }
  | { hello: true }
  /** Home's dashboard, heard by the tab that holds the stream. */
  | { home: HomeFrame }
  /** A tab saying whether it is showing Home; `force` asks for all of it again. */
  | { homeWant: { tab: string; on: boolean; force?: boolean } }
  /** A new stream asking every tab whether it is showing Home. */
  | { homeAsk: true }

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

  // Home rides this stream. Each tab says whether it shows Home; the tab
  // holding the stream switches Home on while any does, and off when none.
  private readonly tab = Math.random().toString(36).slice(2)
  private myWant = false
  private wants = new Map<string, boolean>()
  private streamId: string | null = null
  private homeOn = false
  private homeListeners = new Set<(f: HomeFrame) => void>()

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
    if ('home' in m) {
      for (const fn of this.homeListeners) fn(m.home)
      if (share) this.bc?.postMessage(m)
      return
    }
    if ('homeWant' in m) {
      if (this.es) {
        this.wants.set(m.homeWant.tab, m.homeWant.on)
        this.syncHome(m.homeWant.on && Boolean(m.homeWant.force))
      }
      return
    }
    if ('homeAsk' in m) {
      if (!this.es && this.myWant) this.bc?.postMessage({ homeWant: { tab: this.tab, on: true, force: true } })
      return
    }
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
    // No EventSource (an old embedded browser, or a test DOM): the bell
    // still works from its refetch interval, just without pop-ups.
    if (typeof EventSource === 'undefined') {
      this.emit({ status: 'ended' }, true)
      return
    }
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
    es.addEventListener('ready', (e) => {
      let id: string | null = null
      try { id = (JSON.parse((e as MessageEvent<string>).data) as { stream?: string }).stream ?? null } catch { /* older control plane */ }
      // A new stream: Home is off on it until someone asks again.
      this.streamId = id
      this.homeOn = false
      this.wants.clear()
      if (this.myWant) this.wants.set(this.tab, true)
      this.bc?.postMessage({ homeAsk: true })
      this.syncHome(true)
    })
    es.addEventListener('home', (e) => {
      try { this.emit({ home: JSON.parse((e as MessageEvent<string>).data) as HomeFrame }, true) } catch { /* not ours */ }
    })
    on('ready', () => ({ type: 'ready' }))
    on('read', () => ({ type: 'read' }))
    on('alert', (d) => ({ type: 'alert', alert: d as ConsoleAlert }))
    on('closed', (d) => ({ type: 'closed', seqs: ((d as { seqs?: number[] } | null)?.seqs ?? []) }))
    on('end', () => {
      es.close()
      this.emit({ status: 'ended' }, true)
      return { type: 'end' }
    })
  }

  /** Says whether this tab shows Home; `force` asks for the whole of it again. */
  wantHome(on: boolean, force = false) {
    this.myWant = on
    if (this.es) {
      this.wants.set(this.tab, on)
      this.syncHome(on && force)
    } else {
      this.bc?.postMessage({ homeWant: { tab: this.tab, on, force } })
    }
  }

  onHome(fn: (f: HomeFrame) => void): () => void {
    this.homeListeners.add(fn)
    return () => this.homeListeners.delete(fn)
  }

  /** Held by the tab with the stream: tells the control plane what the tabs want. */
  private syncHome(force: boolean) {
    const id = this.streamId
    if (!id) return
    const want = [...this.wants.values()].some(Boolean)
    if (want && (force || !this.homeOn)) {
      this.homeOn = true
      streamHome(id, true).catch(() => { this.homeOn = false })
    } else if (!want && this.homeOn) {
      this.homeOn = false
      streamHome(id, false).catch(() => { /* the stream closed; nothing is on */ })
    }
  }

  private stop() {
    this.streamId = null
    this.homeOn = false
    this.wants.clear()
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

/**
 * Listens to Home's dashboard on the shared stream while this tab shows it.
 * `want(true)` switches it on (and with `force`, asks for all of it again);
 * `want(false)` lets it go when no other tab shows Home either.
 */
export function subscribeHome(
  onFrame: (f: HomeFrame) => void,
  onStatus: (s: StreamStatus) => void,
): { want: (on: boolean, force?: boolean) => void; stop: () => void } {
  const offFrames = stream.onHome(onFrame)
  const offStream = stream.subscribe((m) => { if ('status' in m) onStatus(m.status) })
  return {
    want: (on, force) => stream.wantHome(on, force),
    stop: () => {
      stream.wantHome(false)
      offFrames()
      offStream()
    },
  }
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
// Desktop notifications still showing, so one whose request is settled can
// be taken down. Bounded: the oldest are forgotten, not closed.
const shown = new Map<number, Notification>()

export function notifyDesktop(a: ConsoleAlert, open: (link: string) => void): void {
  if (a.closed) return
  if (!desktopWanted() || desktopPermission() !== 'granted' || document.visibilityState === 'visible') return
  try {
    const n = new Notification(a.title, { body: a.detail, tag: `argus-alert-${a.seq}`, icon: '/icon.svg' })
    shown.set(a.seq, n)
    if (shown.size > 50) shown.delete(shown.keys().next().value as number)
    n.onclose = () => shown.delete(a.seq)
    n.onclick = () => {
      window.focus()
      if (a.link) open(a.link)
      n.close()
    }
  } catch {
    // Some mobile browsers only notify from a service worker.
  }
}

/** Takes down the desktop notifications for alerts that were closed. */
export function closeDesktop(seqs: number[]): void {
  for (const s of seqs) {
    shown.get(s)?.close()
    shown.delete(s)
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

