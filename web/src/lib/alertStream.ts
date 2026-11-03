import { alertStreamURL, isConfigured, type ConsoleAlert } from '~/lib/live'
import type { StreamEvent, StreamStatus } from '~/lib/alerts'

// Loaded after the first paint: the bell's count needs none of this.

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

