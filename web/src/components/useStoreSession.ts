import { useCallback, useEffect, useRef, useState } from 'react'
import type { DBLevel, Database } from '~/types/domain'

/*
One Redis or RabbitMQ session over the control plane's relay: the same socket
and ticket as a Postgres session, with one JSON answer per request and no row
batches. Requests made before the session is ready wait and go out on ready;
every pending request is answered when the session ends, so nothing hangs.
*/

export interface RedisValue {
  t: 's' | 'e' | 'i' | 'b' | 'n' | 'a'
  v?: string
  q?: boolean
  i?: number
  a?: RedisValue[]
  n?: number
  cut?: boolean
}

export interface RedisKey { name: string; q?: boolean; type: string; ttl: number }

export interface StoreAnswer {
  id: number
  type: 'ready' | 'result' | 'error' | 'closed'
  error?: string
  needsConfirm?: boolean
  ms?: number
  value?: RedisValue
  keys?: RedisKey[]
  cursor?: string
  keyType?: string
  ttl?: number
  total?: number
  truncated?: boolean
  data?: unknown
  engine?: string
  user?: string
  level?: DBLevel
  version?: string
  session?: string
  ops?: string[]
}

export type StoreRequest = Record<string, unknown> & { op: string }

export interface StoreSession {
  status: 'connecting' | 'ready' | 'closed'
  fatal: string | null
  ready: StoreAnswer | null
  ask: (req: StoreRequest) => Promise<StoreAnswer>
  cancel: (id?: number) => void
  /** The id the next request will carry, so a caller can cancel it. */
  lastId: () => number
}

export function useStoreSession(
  database: Database,
  getTicket: (level?: DBLevel) => Promise<{ ticket: string; role: string; level: DBLevel } | null>,
  endpoint: string,
): StoreSession {
  const wsRef = useRef<WebSocket | null>(null)
  const nextId = useRef(1)
  const [status, setStatus] = useState<'connecting' | 'ready' | 'closed'>('connecting')
  const [fatal, setFatal] = useState<string | null>(null)
  const [ready, setReady] = useState<StoreAnswer | null>(null)
  const pending = useRef(new Map<number, (a: StoreAnswer) => void>())
  const queue = useRef<string[]>([])
  const statusRef = useRef(status)
  statusRef.current = status

  const failPending = useCallback((why: string) => {
    queue.current = []
    for (const [id, cb] of pending.current) cb({ id, type: 'closed', error: why })
    pending.current.clear()
  }, [])

  useEffect(() => {
    let ws: WebSocket | null = null
    let disposed = false
    void (async () => {
      const t = await getTicket()
      if (disposed) return
      if (!t) {
        setFatal('could not obtain authorisation for this session')
        setStatus('closed')
        return
      }
      const url = new URL(endpoint, window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      url.searchParams.set('database', database.id)
      url.searchParams.set('principal', `${t.role}@${t.level}`)
      url.searchParams.set('ticket', t.ticket)
      ws = new WebSocket(url.toString())
      wsRef.current = ws
      ws.onmessage = (ev) => {
        if (typeof ev.data !== 'string') return
        const m = JSON.parse(ev.data) as { type: string; ans?: StoreAnswer; data?: string }
        if (m.type === 'ready') {
          setStatus('ready')
          for (const frame of queue.current) ws?.send(frame)
          queue.current = []
          return
        }
        if (m.type === 'error') {
          setFatal(m.data ?? 'the session ended')
          setStatus('closed')
          failPending(m.data ?? 'the session ended')
          return
        }
        const a = m.ans
        if (!a) return
        if (a.type === 'ready') {
          setReady(a)
          return
        }
        if (a.type === 'closed') {
          setFatal(a.error ?? 'the session was closed')
          setStatus('closed')
          failPending(a.error ?? 'the session was closed')
          return
        }
        const cb = pending.current.get(a.id)
        if (cb) {
          pending.current.delete(a.id)
          cb(a)
        }
      }
      ws.onclose = () => {
        setStatus('closed')
        setFatal((f) => f ?? 'the session ended')
        failPending('the session ended')
      }
    })()
    return () => {
      disposed = true
      ws?.close()
      wsRef.current = null
    }
    // A new ticket is a new session, so getTicket is deliberately not a dependency.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [database.id, endpoint])

  const ask = useCallback((req: StoreRequest) => new Promise<StoreAnswer>((resolve) => {
    const id = nextId.current++
    if (statusRef.current === 'closed') {
      resolve({ id, type: 'closed', error: 'the session ended' })
      return
    }
    pending.current.set(id, resolve)
    const frame = JSON.stringify({ type: 'req', req: { ...req, id } })
    const ws = wsRef.current
    if (statusRef.current === 'ready' && ws && ws.readyState === WebSocket.OPEN) ws.send(frame)
    else queue.current.push(frame)
  }), [])

  const cancel = useCallback((id?: number) => {
    const ws = wsRef.current
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'req', req: { op: 'cancel', id: id ?? 0 } }))
  }, [])

  const lastId = useCallback(() => nextId.current - 1, [])

  return { status, fatal, ready, ask, cancel, lastId }
}

/** redis-cli's way of printing a reply, for the command console. */
export function formatRedis(v: RedisValue | undefined, indent = ''): string {
  if (!v) return ''
  switch (v.t) {
    case 's': return v.v ?? ''
    case 'e': return `(error) ${v.v ?? ''}`
    case 'i': return `(integer) ${v.i ?? 0}`
    case 'n': return '(nil)'
    case 'b': return (v.q ? v.v : JSON.stringify(v.v ?? '')) + (v.cut ? ' …(cut)' : '')
    case 'a': {
      const items = v.a ?? []
      if (items.length === 0) return '(empty array)'
      const w = String(items.length).length
      const lines = items.map((e, i) => {
        const n = `${String(i + 1).padStart(w)}) `
        const body = formatRedis(e, indent + ' '.repeat(n.length))
        return (i === 0 ? '' : indent) + n + body
      })
      if (v.cut) lines.push(`${indent}… ${(v.n ?? 0) - items.length} more not shown`)
      return lines.join('\n')
    }
  }
  return ''
}

/** A bulk value as text for a viewer: quoted when not UTF-8, as the agent sent it. */
export function plain(v: RedisValue | undefined): string {
  if (!v) return ''
  if (v.t === 'b' || v.t === 's') return v.v ?? ''
  if (v.t === 'i') return String(v.i ?? 0)
  if (v.t === 'n') return '(nil)'
  return formatRedis(v)
}
