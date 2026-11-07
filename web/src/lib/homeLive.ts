import { useSyncExternalStore } from 'react'
import { api } from '~/lib/api'
import { isConfigured } from '~/lib/live'
import type { StreamStatus } from '~/lib/alerts'
import type { HomeFrame, HomeSnapshot } from '~/types/domain'

/**
 * Home's live dashboard, held once for the tab.
 *
 * The first paint comes from one GET. After that the dashboard rides the
 * alert bell's stream (one connection per browser, see alertStream.ts): the
 * control plane sends the whole of it once, then only the fields that
 * changed. Frames that arrive in the same animation frame are applied
 * together, and a component reads only its own slice, so only the parts that
 * changed draw again.
 *
 * Hidden for 10 s, the tab lets Home go and the stream carries nothing for
 * it; shown again, it asks for all of it and carries on. When no stream can
 * be held, it reads the whole dashboard every 15 s instead, and says so.
 */

export type HomeStatus =
  /** No control plane: the fixture's sample. */
  | 'fixture'
  | 'connecting'
  | 'live'
  /** The tab is hidden and Home is let go. */
  | 'paused'
  /** No stream: read every 15 s. */
  | 'polling'
  /** Signed in as someone who may no longer see the fleet. */
  | 'off'

interface State {
  snap: HomeSnapshot | null
  status: HomeStatus
  /** When the numbers on screen were last known to be current, ms. */
  heardAt: number
}

const PAUSE_AFTER = 10_000
const POLL_EVERY = 15_000
/** How long a stream may stay down before the page reads on its own. */
const POLL_AFTER = 8_000

let state: State = { snap: null, status: 'connecting', heardAt: 0 }
const listeners = new Set<() => void>()
let version = -1
let users = 0
let teardown: (() => void) | null = null

function set(next: Partial<State>) {
  state = { ...state, ...next }
  for (const fn of listeners) fn()
}

// Frames heard in one animation frame are applied together.
let queued: HomeFrame[] = []
let scheduled = false
function flush() {
  scheduled = false
  const frames = queued
  queued = []
  let snap = state.snap
  for (const f of frames) {
    if (f.full) {
      snap = { ...(snap ?? {}), ...f.set } as HomeSnapshot
      version = f.v
    } else if (snap && f.v === version + 1) {
      // Replaces the changed fields only: every other slice keeps its
      // identity, and whoever reads it does not draw again.
      snap = { ...snap, ...f.set }
      version = f.v
    } else {
      // A gap: something was missed, so ask for all of it.
      resync?.()
      break
    }
  }
  set({ snap, heardAt: Date.now(), status: document.visibilityState === 'hidden' ? state.status : 'live' })
}
function schedule() {
  if (scheduled) return
  scheduled = true
  if (typeof requestAnimationFrame === 'function' && document.visibilityState === 'visible') {
    requestAnimationFrame(flush)
  } else {
    setTimeout(flush, 0)
  }
}

let resync: (() => void) | null = null

/** Reads the whole dashboard; `first` keeps whatever the stream sent meanwhile. */
async function fetchWhole(first = false) {
  try {
    const snap = await api.home()
    if (first && state.snap) return
    version = -1
    set({ snap, heardAt: Date.now() })
  } catch {
    // Kept as it was; the pill says how old it is.
  }
}

function start() {
  void fetchWhole(true)
  if (!isConfigured()) {
    set({ status: 'fixture' })
    return () => {}
  }
  let gone = false
  let want: ((on: boolean, force?: boolean) => void) | null = null
  let stopStream: (() => void) | null = null
  let pauseTimer: ReturnType<typeof setTimeout> | null = null
  let pollTimer: ReturnType<typeof setInterval> | null = null
  let downTimer: ReturnType<typeof setTimeout> | null = null

  const stopPolling = () => {
    if (pollTimer) clearInterval(pollTimer)
    pollTimer = null
  }
  const startPolling = () => {
    if (pollTimer || document.visibilityState === 'hidden') return
    set({ status: 'polling' })
    pollTimer = setInterval(() => void fetchWhole(), POLL_EVERY)
  }

  const onStatus = (s: StreamStatus) => {
    if (s === 'live') {
      if (downTimer) clearTimeout(downTimer)
      downTimer = null
      stopPolling()
      return
    }
    if (s === 'reconnecting' || s === 'ended') {
      downTimer ??= setTimeout(startPolling, s === 'ended' ? 0 : POLL_AFTER)
    }
  }
  const onFrame = (f: HomeFrame) => {
    if (f.off) {
      want?.(false)
      set({ status: 'off' })
      return
    }
    stopPolling()
    queued.push(f)
    schedule()
  }

  const onVisibility = () => {
    if (document.visibilityState === 'hidden') {
      pauseTimer = setTimeout(() => {
        want?.(false)
        stopPolling()
        set({ status: 'paused' })
      }, PAUSE_AFTER)
      return
    }
    if (pauseTimer) clearTimeout(pauseTimer)
    pauseTimer = null
    if (state.status === 'paused') {
      set({ status: 'connecting' })
      want?.(true, true)
    }
    if (downTimer && !pollTimer) startPolling()
  }
  document.addEventListener('visibilitychange', onVisibility)
  // A tab being closed lets Home go at once rather than after a pause.
  const onHide = () => want?.(false)
  window.addEventListener('pagehide', onHide)

  void import('~/lib/alertStream').then(({ subscribeHome }) => {
    if (gone) return
    const sub = subscribeHome(onFrame, onStatus)
    want = sub.want
    stopStream = sub.stop
    resync = () => sub.want(true, true)
    if (document.visibilityState !== 'hidden') sub.want(true)
  })

  return () => {
    gone = true
    stopStream?.()
    resync = null
    stopPolling()
    for (const t of [pauseTimer, downTimer]) if (t) clearTimeout(t)
    document.removeEventListener('visibilitychange', onVisibility)
    window.removeEventListener('pagehide', onHide)
  }
}

function subscribe(fn: () => void): () => void {
  listeners.add(fn)
  return () => listeners.delete(fn)
}

/** Holds Home's stream open while at least one caller does. */
export function retainHome(): () => void {
  users++
  if (users === 1) teardown = start()
  return () => {
    users--
    if (users === 0) {
      teardown?.()
      teardown = null
      // The next Home starts from a fresh read, not from what this one left.
      state = { snap: null, status: 'connecting', heardAt: 0 }
      version = -1
    }
  }
}

/**
 * One slice of Home's dashboard. The selector should return a field of the
 * snapshot as it is, so the component draws again only when that changed.
 */
export function useHome<T>(select: (s: HomeSnapshot) => T): T | undefined {
  return useSyncExternalStore(subscribe, () => (state.snap ? select(state.snap) : undefined))
}

export function useHomeStatus(): { status: HomeStatus; heardAt: number } {
  const status = useSyncExternalStore(subscribe, () => state.status)
  const heardAt = useSyncExternalStore(subscribe, () => state.heardAt)
  return { status, heardAt }
}

/** For tests: forget everything. */
export function resetHomeForTests() {
  teardown?.()
  teardown = null
  users = 0
  version = -1
  queued = []
  state = { snap: null, status: 'connecting', heardAt: 0 }
}
