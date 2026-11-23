/*
NATS in the console's terms: the shapes the agent answers with for one
account -- JetStream's own JSON for streams and consumers, and the agent's
for subjects, connections and messages -- and the rules the NATS workspace
applies to them: how subjects match, what needs attention, and how a stream
or consumer form turns into what the agent takes. All of it is pure, so it
is tested without a server.
*/

export interface NatsLimits {
  max_memory: number
  max_storage: number
  max_streams: number
  max_consumers: number
}

export interface NatsAccountInfo {
  memory: number
  storage: number
  streams: number
  consumers: number
  limits?: NatsLimits
  domain?: string
  api?: { total: number; errors: number }
}

export interface NatsAccountStat {
  conns: number
  subscriptions: number
  msgsIn: number
  msgsOut: number
  bytesIn: number
  bytesOut: number
  slowConsumers: number
}

export interface NatsOverview {
  account: string
  accountId: string
  server?: string
  version?: string
  cluster?: string
  jetstream: boolean
  info?: NatsAccountInfo
  stat?: NatsAccountStat
  statError?: string
}

export interface NatsPeer { name: string; current: boolean; active?: number; lag?: number; offline?: boolean }
export interface NatsCluster { name?: string; leader?: string; replicas?: NatsPeer[] }

export interface NatsStreamConfig {
  name: string
  description?: string
  subjects?: string[]
  retention: 'limits' | 'interest' | 'workqueue'
  storage: 'file' | 'memory'
  num_replicas: number
  max_age: number
  max_bytes: number
  max_msgs: number
  max_msgs_per_subject: number
  discard: 'old' | 'new'
  duplicate_window?: number
  deny_delete?: boolean
  deny_purge?: boolean
  sealed?: boolean
  mirror?: { name: string }
  sources?: { name: string }[]
}

export interface NatsStreamState {
  messages: number
  bytes: number
  first_seq: number
  first_ts: string
  last_seq: number
  last_ts: string
  consumer_count: number
  num_subjects?: number
  num_deleted?: number
}

export interface NatsStream {
  config: NatsStreamConfig
  created: string
  state: NatsStreamState
  cluster?: NatsCluster
}

export interface NatsSequenceInfo { consumer_seq: number; stream_seq: number; last_active?: string }

export interface NatsConsumerConfig {
  name?: string
  durable_name?: string
  description?: string
  deliver_policy: string
  opt_start_seq?: number
  opt_start_time?: string
  ack_policy: 'explicit' | 'all' | 'none'
  ack_wait?: number
  max_deliver?: number
  backoff?: number[]
  filter_subject?: string
  filter_subjects?: string[]
  max_ack_pending?: number
  deliver_subject?: string
  deliver_group?: string
}

export interface NatsConsumer {
  stream_name: string
  name: string
  created: string
  config: NatsConsumerConfig
  delivered: NatsSequenceInfo
  ack_floor: NatsSequenceInfo
  num_ack_pending: number
  num_redelivered: number
  num_waiting: number
  num_pending: number
  cluster?: NatsCluster
  push_bound?: boolean
  paused?: boolean
  pause_remaining?: number
}

export interface NatsSubject {
  subject: string
  subs: number
  queues?: string[]
  msgs: number
  apps?: string[]
  streams?: string[]
}

export interface NatsSub { subject: string; queue?: string; msgs: number }

export interface NatsConn {
  cid: number
  name?: string
  lang?: string
  version?: string
  ip: string
  port: number
  start: string
  lastActivity: string
  rtt?: string
  pendingBytes: number
  inMsgs: number
  outMsgs: number
  inBytes: number
  outBytes: number
  subscriptions: number
  user?: string
  argus?: boolean
  subs?: NatsSub[]
}

export interface NatsMessage {
  seq?: number
  subject: string
  time?: string
  headers?: Record<string, string[]>
  data: string
  b64?: boolean
  cut?: boolean
  size: number
}

/** A subject is tokens split by dots; * is one token, > the rest (last only). */
export function validSubject(s: string): boolean {
  if (!s || s.length > 255 || /\s/.test(s)) return false
  const t = s.split('.')
  return t.every((x, i) => x !== '' && (x === '*' || x === '>' || !/[*>]/.test(x)) && (x !== '>' || i === t.length - 1))
}

/** A subject an app publishes to: no wildcards, nothing NATS or Argus keeps for itself. */
export function appSubject(s: string): boolean {
  return validSubject(s) && !/(^|\.)[*>](\.|$)/.test(s) && !s.startsWith('$') && !s.startsWith('_INBOX')
}

/** Whether a filter (with * and >) takes a literal subject. */
export function subjectMatches(filter: string, subject: string): boolean {
  const f = filter.split('.')
  const s = subject.split('.')
  for (let i = 0; i < f.length; i++) {
    if (f[i] === '>') return s.length > i
    if (i >= s.length) return false
    if (f[i] !== '*' && f[i] !== s[i]) return false
  }
  return f.length === s.length
}

/** Whether two filters can both take some subject: what makes two streams clash. */
export function subjectsOverlap(a: string, b: string): boolean {
  const x = a.split('.')
  const y = b.split('.')
  for (let i = 0; i < Math.max(x.length, y.length); i++) {
    if (x[i] === '>' || y[i] === '>') return i < x.length && i < y.length
    if (x[i] === undefined || y[i] === undefined) return false
    if (x[i] !== '*' && y[i] !== '*' && x[i] !== y[i]) return false
  }
  return true
}

/** The streams that store a subject, when someone tests one. */
export function streamsFor(subject: string, streams: NatsStream[]): string[] {
  return streams.filter((s) => (s.config.subjects ?? []).some((f) => subjectMatches(f, subject))).map((s) => s.config.name)
}

export function validName(n: string): boolean {
  return /^[A-Za-z0-9_-]{1,255}$/.test(n)
}

export type NatsAttention = { level: 'rose' | 'amber'; text: string; stream?: string; consumer?: string }

const near = (used: number, max: number) => max > 0 && used / max >= 0.9

/**
 * What an operator would want to know first: a stream short of replicas or
 * without a leader, a stream close to its limit, a consumer messages wait on
 * with nobody pulling, and an account close to what JetStream lets it keep.
 */
export function natsAttention(overview: NatsOverview | null, streams: NatsStream[], consumers: NatsConsumer[]): NatsAttention[] {
  const out: NatsAttention[] = []
  for (const s of streams) {
    const n = s.config.name
    const peers = s.cluster?.replicas ?? []
    if (s.config.num_replicas > 1 && s.cluster && !s.cluster.leader) {
      out.push({ level: 'rose', stream: n, text: `${n} has no leader, so nothing can be stored in it or read from it.` })
    } else if (peers.some((p) => p.offline || !p.current)) {
      const behind = peers.filter((p) => p.offline || !p.current).length
      out.push({ level: 'amber', stream: n, text: `${n}: ${behind} of ${peers.length + 1} replicas ${behind === 1 ? 'is' : 'are'} behind or offline.` })
    }
    const c = s.config
    if (near(s.state.messages, c.max_msgs) || near(s.state.bytes, c.max_bytes)) {
      out.push({ level: 'amber', stream: n, text: c.discard === 'new'
        ? `${n} is nearly full and refuses new messages when it is.`
        : `${n} is nearly full, so the oldest messages are being dropped.` })
    }
  }
  for (const c of consumers) {
    if (c.paused) continue
    if (c.num_pending > 0 && c.num_waiting === 0 && !c.push_bound && !c.config.deliver_subject && idleFor(c) > 5 * 60_000) {
      out.push({ level: 'amber', stream: c.stream_name, consumer: c.name,
        text: `${c.stream_name} › ${c.name}: ${fmtCount(c.num_pending)} message${c.num_pending === 1 ? '' : 's'} wait and no app has pulled for ${ago(c.delivered.last_active)}.` })
    }
    if (c.config.max_deliver && c.config.max_deliver > 0 && c.num_redelivered > 0 && c.num_ack_pending > 0 && c.num_redelivered >= c.num_ack_pending) {
      out.push({ level: 'amber', stream: c.stream_name, consumer: c.name,
        text: `${c.stream_name} › ${c.name}: ${fmtCount(c.num_redelivered)} message${c.num_redelivered === 1 ? ' is' : 's are'} being delivered again; an app may be failing them.` })
    }
  }
  const info = overview?.info
  if (info?.limits) {
    if (near(info.storage, info.limits.max_storage)) out.push({ level: 'amber', text: `The account has used ${fmtBytes(info.storage)} of the ${fmtBytes(info.limits.max_storage)} of file storage it may.` })
    if (near(info.memory, info.limits.max_memory)) out.push({ level: 'amber', text: `The account has used ${fmtBytes(info.memory)} of the ${fmtBytes(info.limits.max_memory)} of memory it may.` })
  }
  if ((overview?.stat?.slowConsumers ?? 0) > 0) {
    out.push({ level: 'amber', text: `${overview!.stat!.slowConsumers} slow consumer${overview!.stat!.slowConsumers === 1 ? '' : 's'}: the server dropped messages for an app that could not keep up.` })
  }
  return out
}

function idleFor(c: NatsConsumer, now = Date.now()): number {
  const t = c.delivered.last_active ? Date.parse(c.delivered.last_active) : Date.parse(c.created)
  return Number.isFinite(t) ? now - t : 0
}

/** How long ago, in words: "4 min", "2 h", "3 days". */
export function ago(iso: string | undefined, now = Date.now()): string {
  if (!iso) return 'ever'
  const ms = now - Date.parse(iso)
  if (!Number.isFinite(ms) || ms < 0) return 'now'
  if (ms < 60_000) return `${Math.max(1, Math.round(ms / 1000))} s`
  if (ms < 3_600_000) return `${Math.round(ms / 60_000)} min`
  if (ms < 86_400_000) return `${Math.round(ms / 3_600_000)} h`
  return `${Math.round(ms / 86_400_000)} days`
}

export function fmtCount(n: number | undefined): string {
  if (n === undefined || !Number.isFinite(n)) return '–'
  if (n < 10_000) return n.toLocaleString()
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 100_000 ? 1 : 0)}k`
  if (n < 1_000_000_000) return `${(n / 1_000_000).toFixed(n < 10_000_000 ? 2 : 1)}M`
  return `${(n / 1_000_000_000).toFixed(2)}B`
}

export function fmtBytes(n: number | undefined): string {
  if (n === undefined || !Number.isFinite(n) || n < 0) return '–'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++ }
  return `${i === 0 ? v : v.toFixed(v < 10 ? 1 : 0)} ${u[i]}`
}

/** A nanosecond duration as people say it: "7 days", "2 min"; 0 is "for ever". */
export function fmtNanos(ns: number | undefined, zero = 'for ever'): string {
  if (!ns) return zero
  const s = ns / 1e9
  if (s % 86_400 === 0) return `${s / 86_400} day${s === 86_400 ? '' : 's'}`
  if (s % 3600 === 0) return `${s / 3600} h`
  if (s % 60 === 0) return `${s / 60} min`
  return `${+s.toFixed(3)} s`
}

/** A stream's limit, or "none" where NATS says -1 or 0. */
export function fmtLimit(n: number | undefined, f: (n: number) => string = fmtCount): string {
  return n === undefined || n <= 0 ? 'none' : f(n)
}

/** A message's data as it is best read: JSON indented, text as is, binary said so. */
export function showData(m: Pick<NatsMessage, 'data' | 'b64' | 'cut'>): { text: string; kind: 'json' | 'text' | 'base64' } {
  if (m.b64) return { text: m.data, kind: 'base64' }
  if (!m.cut) {
    const t = m.data.trim()
    if ((t.startsWith('{') && t.endsWith('}')) || (t.startsWith('[') && t.endsWith(']'))) {
      try { return { text: JSON.stringify(JSON.parse(t), null, 2), kind: 'json' } } catch { /* not JSON */ }
    }
  }
  return { text: m.data, kind: 'text' }
}

/** The filters a consumer reads, whichever field holds them. */
export function consumerFilters(c: NatsConsumerConfig): string[] {
  return c.filter_subjects?.length ? c.filter_subjects : c.filter_subject ? [c.filter_subject] : []
}

/** Pull or push, and for push, whether a queue group shares it. */
export function consumerKind(c: NatsConsumerConfig): string {
  if (!c.deliver_subject) return 'pull'
  return c.deliver_group ? `push · ${c.deliver_group}` : 'push'
}

/** What the stream form sends: the agent's NATSStreamSpec. */
export interface StreamSpec {
  name: string
  subjects: string[]
  description?: string
  retention?: 'limits' | 'interest' | 'workqueue'
  storage?: 'file' | 'memory'
  replicas?: number
  maxAgeS?: number
  maxBytes?: number
  maxMsgs?: number
  maxPerSubject?: number
  discard?: 'old' | 'new'
  duplicatesS?: number
  allowDelete?: boolean
  denyPurge?: boolean
}

/** A stream as the form starts with it, to change it. */
export function specOf(c: NatsStreamConfig): StreamSpec {
  return {
    name: c.name,
    subjects: c.subjects ?? [],
    description: c.description ?? '',
    retention: c.retention,
    storage: c.storage,
    replicas: c.num_replicas || 1,
    maxAgeS: c.max_age > 0 ? Math.round(c.max_age / 1e9) : 0,
    maxBytes: c.max_bytes > 0 ? c.max_bytes : 0,
    maxMsgs: c.max_msgs > 0 ? c.max_msgs : 0,
    maxPerSubject: c.max_msgs_per_subject > 0 ? c.max_msgs_per_subject : 0,
    discard: c.discard,
    duplicatesS: c.duplicate_window ? Math.round(c.duplicate_window / 1e9) : 0,
    allowDelete: !c.deny_delete,
    denyPurge: Boolean(c.deny_purge),
  }
}

/**
 * What is wrong with a stream form before the agent is asked, in the
 * order a person would fix it. The agent checks all of it again.
 */
export function streamProblems(s: StreamSpec, others: NatsStream[]): string[] {
  const out: string[] = []
  if (!validName(s.name)) out.push('The name is letters, digits, - and _ only.')
  const subs = s.subjects.map((x) => x.trim()).filter(Boolean)
  if (subs.length === 0) out.push('A stream needs at least one subject.')
  for (const x of subs) {
    if (!validSubject(x)) out.push(`${x} is not a subject.`)
    else if (x === '>') out.push('A stream cannot take every subject: it would store the account’s own replies.')
    else if (x.startsWith('$') || x.startsWith('_INBOX')) out.push(`${x} is kept for NATS itself.`)
  }
  for (const o of others) {
    if (o.config.name === s.name) continue
    for (const x of subs) {
      const hit = (o.config.subjects ?? []).find((f) => validSubject(x) && subjectsOverlap(f, x))
      if (hit) out.push(`${x} overlaps ${hit}, which ${o.config.name} already stores. NATS refuses two streams on one subject.`)
    }
  }
  if (s.replicas !== undefined && (s.replicas < 1 || s.replicas > 5)) out.push('Replicas is 1 to 5.')
  return out
}

/** What the consumer form sends: the agent's NATSConsumerSpec. */
export interface ConsumerSpec {
  name: string
  description?: string
  filterSubjects?: string[]
  deliver?: 'all' | 'new' | 'last' | 'last_per_subject' | 'by_start_sequence' | 'by_start_time'
  startSeq?: number
  startTime?: string
  ack?: 'explicit' | 'all' | 'none'
  ackWaitS?: number
  maxDeliver?: number
  backoffS?: number[]
  maxAckPending?: number
  deliverSubject?: string
  deliverGroup?: string
}

/** "5, 30, 120" as seconds between tries; null when it is not that. */
export function parseBackoff(t: string): number[] | null {
  if (!t.trim()) return []
  const parts = t.split(/[,\s]+/).filter(Boolean).map(Number)
  return parts.every((n) => Number.isInteger(n) && n > 0) ? parts : null
}

export function consumerProblems(c: ConsumerSpec, stream?: NatsStream): string[] {
  const out: string[] = []
  if (!validName(c.name)) out.push('The name is letters, digits, - and _ only.')
  for (const f of c.filterSubjects ?? []) {
    if (!validSubject(f)) out.push(`${f} is not a subject.`)
    else if (stream && !(stream.config.subjects ?? []).some((s) => subjectsOverlap(s, f))) out.push(`${stream.config.name} stores nothing on ${f}.`)
  }
  if (c.deliver === 'by_start_sequence' && !(c.startSeq && c.startSeq > 0)) out.push('Say the sequence to start at.')
  if (c.deliver === 'by_start_time' && !(c.startTime && Number.isFinite(Date.parse(c.startTime)))) out.push('Say the time to start at.')
  if (c.deliverGroup && !c.deliverSubject) out.push('A queue group needs a push subject.')
  if (c.deliverSubject && !appSubject(c.deliverSubject)) out.push('The push subject is an app’s subject, with no wildcards.')
  if (stream?.config.retention === 'workqueue' && c.ack && c.ack !== 'explicit') out.push('A work-queue stream takes only consumers that ack each message.')
  return out
}

export function items<T>(d: unknown): T[] {
  return Array.isArray(d) ? (d as T[]) : []
}
