/*
RabbitMQ in the broker's own terms: the shapes the management API answers
with, and the few rules the broker workspace applies to them -- which queues
a routing key reaches, what needs attention, and what an x-death header says.
All of it is pure, so it is tested without a broker.
*/

export interface Rate { rate?: number; samples?: { sample: number; timestamp: number }[] }

export interface MessageStats {
  publish?: number
  publish_details?: Rate
  deliver_get_details?: Rate
  ack_details?: Rate
  publish_in_details?: Rate
  publish_out_details?: Rate
  drop_unroutable_details?: Rate
  return_unroutable_details?: Rate
}

export interface Queue {
  name: string
  type?: string
  durable?: boolean
  auto_delete?: boolean
  exclusive?: boolean
  arguments?: Record<string, unknown>
  messages?: number
  messages_ready?: number
  messages_unacknowledged?: number
  messages_ready_details?: Rate
  consumers?: number
  state?: string
  idle_since?: string
  policy?: string | null
  message_stats?: MessageStats
  consumer_details?: Consumer[]
}

export interface Exchange {
  name: string
  type?: string
  durable?: boolean
  internal?: boolean
  auto_delete?: boolean
  arguments?: Record<string, unknown>
  message_stats?: MessageStats
  incoming?: { stats?: { publish?: number; publish_details?: Rate }; channel_details?: ChannelRef }[]
}

export interface Binding {
  source: string
  destination: string
  destination_type?: 'queue' | 'exchange'
  routing_key: string
  arguments?: Record<string, unknown>
  properties_key?: string
}

export interface ChannelRef { name?: string; connection_name?: string; peer_host?: string; user?: string; number?: number }

export interface Consumer {
  consumer_tag?: string
  prefetch_count?: number
  ack_required?: boolean
  active?: boolean
  queue?: { name: string }
  channel_details?: ChannelRef
}

export interface Connection {
  name: string
  user?: string
  peer_host?: string
  peer_port?: number
  state?: string
  channels?: number
  protocol?: string
  connected_at?: number
  client_properties?: { product?: string; version?: string; platform?: string; connection_name?: string }
}

export interface Channel {
  name: string
  number?: number
  connection_details?: { name?: string; peer_host?: string }
  prefetch_count?: number
  messages_unacknowledged?: number
  consumer_count?: number
  confirm?: boolean
  state?: string
  message_stats?: MessageStats
}

export interface Node {
  name: string
  running?: boolean
  uptime?: number
  mem_used?: number
  mem_limit?: number
  mem_alarm?: boolean
  disk_free?: number
  disk_free_limit?: number
  disk_free_alarm?: boolean
  fd_used?: number
  fd_total?: number
}

export interface VhostStats {
  name?: string
  messages?: number
  messages_ready?: number
  messages_unacknowledged?: number
  messages_ready_details?: Rate
  message_stats?: MessageStats
}

export interface Peeked {
  payload: string
  payload_encoding?: string
  payload_bytes?: number
  routing_key?: string
  exchange?: string
  redelivered?: boolean
  properties?: Record<string, unknown> & { headers?: Record<string, unknown> }
  message_count?: number
}

/** A list answer: the management API pages some lists and not others. */
export function items<T>(data: unknown): T[] {
  if (Array.isArray(data)) return data as T[]
  if (data && typeof data === 'object' && Array.isArray((data as { items?: unknown }).items)) return (data as { items: T[] }).items
  return []
}

export const rate = (r?: Rate) => r?.rate ?? 0

/** A rate as people read it: 1,240/s, 3.5/s, 0/s. */
export function fmtRate(n: number): string {
  if (n === 0) return '0/s'
  if (n < 10) return `${Math.round(n * 10) / 10}/s`
  return `${Math.round(n).toLocaleString()}/s`
}

export function fmtCount(n: number | undefined): string {
  if (n === undefined) return '—'
  if (n >= 1_000_000) return `${Math.round(n / 100_000) / 10} M`
  return n.toLocaleString()
}

export function fmtBytes(n: number | undefined): string {
  if (n === undefined) return '—'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++ }
  return `${i === 0 ? v : Math.round(v * 10) / 10} ${u[i]}`
}

/** Samples oldest first, as a chart draws them. RabbitMQ sends newest first. */
export function samples(r?: Rate): number[] {
  return (r?.samples ?? []).slice().reverse().map((s) => s.sample)
}

/**
 * Per-second rates from cumulative samples: RabbitMQ's message_stats samples
 * are running totals, so a chart of a rate is the difference of neighbours.
 */
export function rateSeries(r?: Rate): number[] {
  const s = (r?.samples ?? []).slice().reverse()
  const out: number[] = []
  for (let i = 1; i < s.length; i++) {
    const a = s[i - 1]!
    const b = s[i]!
    const dt = (b.timestamp - a.timestamp) / 1000
    out.push(dt > 0 ? Math.max(0, (b.sample - a.sample) / dt) : 0)
  }
  return out
}

// ---------------------------------------------------------------- routing

/** Whether a topic binding key matches a routing key: `*` is one word, `#` any number. */
export function topicMatches(pattern: string, key: string): boolean {
  const p = pattern.split('.')
  const k = key === '' ? [] : key.split('.')
  const memo = new Map<string, boolean>()
  const go = (i: number, j: number): boolean => {
    const id = `${i},${j}`
    const hit = memo.get(id)
    if (hit !== undefined) return hit
    let r: boolean
    if (i === p.length) r = j === k.length
    else if (p[i] === '#') r = go(i + 1, j) || (j < k.length && go(i, j + 1))
    else r = j < k.length && (p[i] === '*' || p[i] === k[j]) && go(i + 1, j + 1)
    memo.set(id, r)
    return r
  }
  return go(0, 0)
}

/** Whether a headers-exchange binding matches a message's headers. */
export function headersMatch(args: Record<string, unknown> | undefined, headers: Record<string, string>): boolean {
  const a = args ?? {}
  const mode = String(a['x-match'] ?? 'all')
  const withX = mode.endsWith('-with-x')
  const want = Object.entries(a).filter(([k]) => k !== 'x-match' && (withX || !k.startsWith('x-')))
  if (want.length === 0) return mode.startsWith('all')
  const ok = (k: string, v: unknown) => k in headers && (v === '' || v === null || String(v) === headers[k])
  return mode.startsWith('any') ? want.some(([k, v]) => ok(k, v)) : want.every(([k, v]) => ok(k, v))
}

/** Whether one binding takes a message published to its source exchange. */
export function bindingTakes(type: string | undefined, b: Binding, key: string, headers: Record<string, string>): boolean {
  switch (type) {
    case 'fanout': return true
    case 'topic': return topicMatches(b.routing_key, key)
    case 'headers': return headersMatch(b.arguments, headers)
    default: return b.routing_key === key
  }
}

export interface Route { queue: string; via: Binding[] }

/**
 * The queues a message published to `exchange` with `key` reaches, following
 * exchange-to-exchange bindings and the alternate exchange, as RabbitMQ
 * would. Nothing is published. The default exchange (`''`) routes by queue
 * name to any queue that exists.
 */
export function routeKey(
  exchange: string, key: string, headers: Record<string, string>,
  exchanges: Exchange[], bindings: Binding[], queues: Queue[],
): Route[] {
  if (exchange === '' || exchange === 'amq.default') {
    return queues.some((q) => q.name === key) ? [{ queue: key, via: [] }] : []
  }
  const types = new Map(exchanges.map((x) => [x.name, x]))
  const out = new Map<string, Route>()
  const seen = new Set<string>()
  const walk = (ex: string, via: Binding[]) => {
    if (seen.has(ex)) return
    seen.add(ex)
    const x = types.get(ex)
    let routed = false
    for (const b of bindings) {
      if (b.source !== ex || !bindingTakes(x?.type, b, key, headers)) continue
      routed = true
      if (b.destination_type === 'exchange') walk(b.destination, [...via, b])
      else if (!out.has(b.destination)) out.set(b.destination, { queue: b.destination, via: [...via, b] })
    }
    const alt = x?.arguments?.['alternate-exchange']
    if (!routed && typeof alt === 'string' && alt) walk(alt, via)
  }
  walk(exchange, [])
  return [...out.values()]
}

// ---------------------------------------------------------------- dead letters

/** The exchanges some queue dead-letters to. */
export function deadLetterExchanges(queues: Queue[]): Set<string> {
  const s = new Set<string>()
  for (const q of queues) {
    const x = q.arguments?.['x-dead-letter-exchange']
    if (typeof x === 'string') s.add(x)
  }
  return s
}

/**
 * Queues that receive dead letters: bound to a dead-letter exchange, or
 * named as the target of the default exchange by x-dead-letter-routing-key.
 * Policies can set dead-lettering too; those are not visible here, so a
 * queue named like a dead-letter queue counts as well.
 */
export function deadLetterQueues(queues: Queue[], bindings: Binding[]): Map<string, string> {
  const out = new Map<string, string>() // dlq -> where its letters come from
  const dlx = new Map<string, string[]>()
  for (const q of queues) {
    const x = q.arguments?.['x-dead-letter-exchange']
    if (typeof x !== 'string') continue
    if (x === '') {
      const rk = q.arguments?.['x-dead-letter-routing-key']
      if (typeof rk === 'string') out.set(rk, q.name)
      continue
    }
    dlx.set(x, [...(dlx.get(x) ?? []), q.name])
  }
  for (const b of bindings) {
    const from = dlx.get(b.source)
    if (from && b.destination_type !== 'exchange') out.set(b.destination, from.join(', '))
  }
  for (const q of queues) {
    if (!out.has(q.name) && /(^|[._-])(dlq|dead|deadletter|dead-letter|parking|failed)([._-]|$)/i.test(q.name)) out.set(q.name, '')
  }
  return out
}

export interface Death { reason: string; queue: string; count: number; exchange?: string; keys?: string[] }

/** The x-death header, newest first, as words can say it. */
export function xDeath(p?: Peeked['properties']): Death[] {
  const h = p?.headers?.['x-death']
  if (!Array.isArray(h)) return []
  return h.map((d) => {
    const o = (d ?? {}) as Record<string, unknown>
    return {
      reason: String(o.reason ?? ''), queue: String(o.queue ?? ''), count: Number(o.count ?? 1),
      exchange: o.exchange === undefined ? undefined : String(o.exchange),
      keys: Array.isArray(o['routing-keys']) ? (o['routing-keys'] as unknown[]).map(String) : undefined,
    }
  })
}

/** Where a dead-lettered message came from, to send it back. */
export function cameFrom(msgs: Peeked[]): string | null {
  const counts = new Map<string, number>()
  for (const m of msgs) {
    const q = xDeath(m.properties)[0]?.queue
    if (q) counts.set(q, (counts.get(q) ?? 0) + 1)
  }
  let best: string | null = null
  let n = 0
  for (const [q, c] of counts) if (c > n) { best = q; n = c }
  return best
}

// ---------------------------------------------------------------- attention

export type Tone = 'rose' | 'amber' | 'sky'
export interface Attention { tone: Tone; queue?: string; title: string; detail: string; action?: 'queue' | 'routing' | 'connections' }

/** A queue whose consumers are falling behind. */
export function growing(q: Queue): boolean {
  const inr = rate(q.message_stats?.publish_details)
  const out = rate(q.message_stats?.deliver_get_details)
  return (q.messages_ready ?? 0) > 100 && inr > 0 && inr > out * 1.1
}

export function noConsumers(q: Queue): boolean {
  return (q.messages_ready ?? 0) > 0 && (q.consumers ?? 0) === 0 && q.type !== 'stream'
}

/** What the overview lists under "Needs attention", worst first. */
export function attention(queues: Queue[], bindings: Binding[], vhost: VhostStats | null, nodes: Node[] | null): Attention[] {
  const out: Attention[] = []
  for (const n of nodes ?? []) {
    if (n.mem_alarm) out.push({ tone: 'rose', title: `Memory alarm on ${n.name}`, detail: 'RabbitMQ has stopped reading from every publisher until memory drops.' })
    if (n.disk_free_alarm) out.push({ tone: 'rose', title: `Disk alarm on ${n.name}`, detail: 'RabbitMQ has stopped reading from every publisher until disk is freed.' })
  }
  const dlq = deadLetterQueues(queues, bindings)
  for (const q of queues) {
    if (dlq.has(q.name)) continue
    if (noConsumers(q)) {
      out.push({ tone: 'rose', queue: q.name, action: 'queue', title: `${q.name} has no consumers`,
        detail: `${fmtCount(q.messages_ready)} ready${rate(q.message_stats?.publish_details) > 0 ? `, ${fmtRate(rate(q.message_stats?.publish_details))} coming in` : ''}.` })
    } else if (growing(q)) {
      out.push({ tone: 'amber', queue: q.name, action: 'queue', title: `${q.name} is falling behind`,
        detail: `${fmtRate(rate(q.message_stats?.publish_details))} in, ${fmtRate(rate(q.message_stats?.deliver_get_details))} out, ${fmtCount(q.messages_ready)} ready.` })
    } else if (q.state === 'flow') {
      out.push({ tone: 'amber', queue: q.name, action: 'queue', title: `${q.name} is in flow control`, detail: 'RabbitMQ is slowing its publishers down.' })
    }
  }
  for (const q of queues) {
    const from = dlq.get(q.name)
    if (from === undefined || !(q.messages_ready ?? 0)) continue
    out.push({ tone: 'amber', queue: q.name, action: 'queue', title: `${q.name} is collecting dead letters`,
      detail: `${fmtCount(q.messages_ready)} messages${from ? ` from ${from}` : ''}.` })
  }
  const drop = rate(vhost?.message_stats?.drop_unroutable_details) + rate(vhost?.message_stats?.return_unroutable_details)
  if (drop > 0) {
    out.push({ tone: 'sky', action: 'routing', title: 'Messages are matching no binding',
      detail: `${fmtRate(drop)} published to an exchange reached no queue, and RabbitMQ dropped or returned them.` })
  }
  const rank: Record<Tone, number> = { rose: 0, amber: 1, sky: 2 }
  return out.sort((a, b) => rank[a.tone] - rank[b.tone])
}

/** Queues as the table orders them: what needs attention first, then busiest. */
export function sortQueues(queues: Queue[], dlq: Map<string, string>): Queue[] {
  const score = (q: Queue) => (noConsumers(q) && !dlq.has(q.name) ? 3 : 0) + (growing(q) ? 2 : 0) +
    (dlq.has(q.name) && (q.messages_ready ?? 0) > 0 ? 1 : 0) + (q.state === 'flow' ? 1 : 0)
  return queues.slice().sort((a, b) => score(b) - score(a) || rate(b.message_stats?.publish_details) - rate(a.message_stats?.publish_details) || a.name.localeCompare(b.name))
}

/** The feature tags a queue's table row shows. */
export function queueTags(q: Queue, dlq: Map<string, string>): { label: string; dl?: boolean }[] {
  const a = q.arguments ?? {}
  const t: { label: string; dl?: boolean }[] = []
  if (dlq.has(q.name)) t.push({ label: 'dead-letter', dl: true })
  if (a['x-dead-letter-exchange'] !== undefined) t.push({ label: 'DLX', dl: true })
  if (typeof a['x-message-ttl'] === 'number') t.push({ label: `TTL ${fmtMs(a['x-message-ttl'] as number)}` })
  if (typeof a['x-max-length'] === 'number') t.push({ label: `max ${fmtCount(a['x-max-length'] as number)}` })
  if (typeof a['x-max-age'] === 'string') t.push({ label: String(a['x-max-age']) })
  if (a['x-queue-mode'] === 'lazy') t.push({ label: 'lazy' })
  if (q.exclusive) t.push({ label: 'exclusive' })
  if (q.auto_delete) t.push({ label: 'auto-delete' })
  return t
}

function fmtMs(ms: number): string {
  if (ms % 3_600_000 === 0) return `${ms / 3_600_000}h`
  if (ms % 60_000 === 0) return `${ms / 60_000}m`
  if (ms % 1000 === 0) return `${ms / 1000}s`
  return `${ms}ms`
}

/** Whether a queue name is one RabbitMQ made for a client (amq.gen-…). */
export const systemQueue = (name: string) => name.startsWith('amq.')

/** A peeked payload as people read it: pretty JSON when it is JSON. */
export function prettyPayload(m: Peeked): { text: string; json: boolean } {
  if (m.payload_encoding === 'base64') return { text: m.payload, json: false }
  try {
    return { text: JSON.stringify(JSON.parse(m.payload), null, 2), json: true }
  } catch {
    return { text: m.payload, json: false }
  }
}
