import { useCallback, useEffect, useSyncExternalStore } from 'react'
import { useQueryClient, type QueryClient } from '@tanstack/react-query'
import { isConfigured, usageLiveURL } from '~/lib/live'
import type { ResourceWarning, ServerSystem, ServerUsage } from '~/types/domain'

/*
 * A server's usage every couple of seconds while its page is open, straight
 * from its agent (internal/control/usage_live.go). Each sample replaces the
 * System query's usage, so the System card and the Metrics tab's Now tiles
 * move with it, and the last few minutes are kept here for the Metrics tab's
 * live chart. Nothing of it is stored by the control plane.
 *
 * One stream per server however many parts of the page read it. It closes
 * while the tab is hidden, so the agent stops sampling for a page nobody is
 * looking at, and when the control plane says live figures cannot be had
 * here (no agent, an older agent, its connection held by another replica)
 * the page keeps the heartbeat's figures, polled as before.
 */

/** How much of the recent past the live chart keeps. */
export const LIVE_WINDOW_MS = 5 * 60_000
const MAX_SAMPLES = 200
/** After the stream drops, how long before trying again. */
const RETRY_MS = 15_000

export interface LiveSample {
  at: number
  usage: ServerUsage
}

export type LiveState = 'off' | 'connecting' | 'live' | 'unavailable'

export interface LiveUsage {
  state: LiveState
  /** Why live figures are not shown, when state is 'unavailable'. */
  reason: string
  /** Seconds between samples, as the control plane says. */
  every: number
  samples: LiveSample[]
}

interface Wire {
  usage: ServerUsage
  usageAt: string
  warnings: ResourceWarning[]
  everySeconds: number
}

interface Feed {
  id: string
  refs: number
  value: LiveUsage
  listeners: Set<() => void>
  es: EventSource | null
  retry: ReturnType<typeof setTimeout> | null
  client: QueryClient
}

const OFF: LiveUsage = { state: 'off', reason: '', every: 2, samples: [] }
const feeds = new Map<string, Feed>()

function set(f: Feed, v: Partial<LiveUsage>) {
  f.value = { ...f.value, ...v }
  f.listeners.forEach((l) => l())
}

function open(f: Feed) {
  if (f.es || f.refs === 0 || document.visibilityState === 'hidden') return
  if (typeof EventSource === 'undefined') return set(f, { state: 'unavailable', reason: 'this browser cannot receive live figures' })
  const es = new EventSource(usageLiveURL(f.id), { withCredentials: true })
  f.es = es
  if (f.value.state !== 'live') set(f, { state: 'connecting' })
  es.addEventListener('usage', (e) => {
    let w: Wire
    try { w = JSON.parse((e as MessageEvent<string>).data) as Wire } catch { return }
    const at = Date.parse(w.usageAt)
    const keep = f.value.samples.filter((s) => s.at > at - LIVE_WINDOW_MS && s.at < at)
    keep.push({ at, usage: w.usage })
    set(f, { state: 'live', reason: '', every: w.everySeconds || 2, samples: keep.slice(-MAX_SAMPLES) })
    f.client.setQueryData<ServerSystem>(['asset', f.id, 'system'], (prev) =>
      prev ? {
        ...prev, reported: true, usage: w.usage, usageAt: w.usageAt,
        // The processor warning is the heartbeat's, over a minute; live
        // samples carry none (two busy seconds are not running low).
        warnings: [...(w.warnings ?? []), ...(prev.warnings ?? []).filter((x) => x.resource === 'cpu')],
      } : prev)
  })
  es.addEventListener('end', (e) => {
    let reason = ''
    try { reason = (JSON.parse((e as MessageEvent<string>).data) as { reason?: string }).reason ?? '' } catch { /* none */ }
    close(f)
    set(f, { state: 'unavailable', reason: endWords(reason) })
    // An agent that stopped sending is usually restarting or reconnecting:
    // look again later. The others will not change while the page is open.
    if (reason === 'the agent stopped sending live figures' || reason === 'busy') later(f)
  })
  es.onerror = () => {
    if (es.readyState !== EventSource.CLOSED) return // the browser reconnects by itself
    close(f)
    set(f, { state: 'unavailable', reason: 'the connection to the control plane dropped' })
    later(f)
  }
}

function later(f: Feed) {
  if (f.retry) clearTimeout(f.retry)
  f.retry = setTimeout(() => { f.retry = null; open(f) }, RETRY_MS)
}

function close(f: Feed) {
  f.es?.close()
  f.es = null
}

/** The control plane's reason, as the page says it. */
export function endWords(reason: string): string {
  switch (reason) {
    case 'not connected': return 'the agent is not connected to this control plane'
    case 'agent too old': return 'this agent is older than live figures; update it to see them'
    case 'no agent': return 'no agent reports for this server'
    case 'busy': return 'too many live views are open on the control plane'
    case 'signed out': return 'you were signed out'
    case 'the agent stopped sending live figures': return 'the agent stopped sending live figures'
    default: return reason.startsWith('refused: ') ? `the agent refused: ${reason.slice(9)}` : 'the agent could not be reached'
  }
}

function onVisibility() {
  for (const f of feeds.values()) {
    if (document.visibilityState === 'hidden') close(f)
    else open(f)
  }
}

function subscribe(id: string, client: QueryClient): Feed {
  let f = feeds.get(id)
  if (!f) {
    f = { id, refs: 0, value: OFF, listeners: new Set(), es: null, retry: null, client }
    feeds.set(id, f)
    if (feeds.size === 1) document.addEventListener('visibilitychange', onVisibility)
  }
  f.refs++
  open(f)
  return f
}

function unsubscribe(f: Feed) {
  if (--f.refs > 0) return
  close(f)
  if (f.retry) clearTimeout(f.retry)
  feeds.delete(f.id)
  if (feeds.size === 0) document.removeEventListener('visibilitychange', onVisibility)
}

/** Whether the System query for this server is being kept current by a live stream. */
export function isLiveUsage(id: string): boolean {
  return feeds.get(id)?.value.state === 'live'
}

/**
 * Live usage for one server while the calling component is mounted. Off
 * (and nothing opened) when `enabled` is false or no control plane is set.
 */
export function useLiveUsage(id: string, enabled: boolean): LiveUsage {
  const client = useQueryClient()
  const on = enabled && isConfigured()
  useEffect(() => {
    if (!on) return
    const f = subscribe(id, client)
    return () => unsubscribe(f)
  }, [id, on, client])
  const listen = useCallback((l: () => void) => {
    const f = on ? feeds.get(id) : undefined
    if (!f) return () => {}
    f.listeners.add(l)
    return () => { f.listeners.delete(l) }
  }, [id, on])
  return useSyncExternalStore(listen, () => (on ? feeds.get(id)?.value ?? OFF : OFF))
}
