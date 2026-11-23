/*
RabbitMQ streams in the console: reading a stream from an offset or a time,
its readers, and super streams. The agent reads as a reader would and takes
nothing; these are the pure parts the screens use.
*/

export type StreamFrom = 'first' | 'last' | 'offset' | 'time'

export interface StreamRead { from: StreamFrom; offset?: number; at?: number }

export interface StreamMessage {
  offset: number
  time?: number
  properties?: Record<string, unknown>
  headers?: Record<string, unknown>
  body: string
  encoding: 'string' | 'base64'
  size: number
  truncated?: boolean
}

export interface StreamPage { messages: StreamMessage[]; first: number; last: number; empty?: boolean }

export interface StreamReader {
  name: string
  protocol: 'stream' | 'amqp'
  offset?: number
  behind?: number
  connection?: string
  from?: string
  user?: string
}

export interface StreamReaders { readers: StreamReader[]; publishers: StreamReader[]; noPlugin?: boolean }

export interface SuperStreamPartition { stream: string; routingKey: string; order: number; messages: number; missing?: boolean }
export interface SuperStream { name: string; partitions: SuperStreamPartition[]; messages: number }

export const MAX_STREAM_READ = 200
export const MAX_SUPER_PARTITIONS = 64

/** Where Older and Newer start from the page shown, or null at an end. */
export function pageMoves(page: StreamPage, count: number): { older: number | null; newer: number | null } {
  const lo = page.messages[0]?.offset
  const hi = page.messages[page.messages.length - 1]?.offset
  if (page.empty || lo === undefined || hi === undefined) return { older: null, newer: null }
  return {
    older: lo > page.first ? Math.max(page.first, lo - count) : null,
    newer: hi < page.last ? hi + 1 : null,
  }
}

/** "48,211 to 48,230 of 0 to 53,083". */
export function pageWords(page: StreamPage): string {
  if (page.empty) return 'The stream holds no messages.'
  const n = (v: number) => v.toLocaleString('en-US')
  const ends = `${n(page.first)} to ${n(page.last)}`
  const lo = page.messages[0]?.offset
  const hi = page.messages[page.messages.length - 1]?.offset
  if (lo === undefined || hi === undefined) return `No messages there. The stream holds offsets ${ends}.`
  return `${n(lo)} to ${n(hi)} of ${ends}`
}

/** A message's body for reading: JSON indented, anything else as it is. */
export function streamBody(m: StreamMessage): { text: string; json: boolean } {
  if (m.encoding === 'base64') return { text: m.body, json: false }
  try {
    return { text: JSON.stringify(JSON.parse(m.body), null, 2), json: true }
  } catch {
    return { text: m.body, json: false }
  }
}

/** Routing keys typed one per line or separated by commas. */
export function parseKeys(s: string): string[] {
  return s.split(/[\n,]/).map((k) => k.trim()).filter(Boolean)
}

/** The routing keys of a new super stream's partitions, in order. */
export function superStreamKeys(by: 'hash' | 'keys', partitions: number, keys: string[]): string[] {
  if (by === 'keys') return keys
  return Array.from({ length: Math.max(0, Math.floor(partitions)) }, (_, i) => String(i))
}

const NAME_RE = /^[A-Za-z0-9._-]{1,200}$/
const KEY_RE = /^[A-Za-z0-9._-]{1,50}$/

/** Why a super stream cannot be made as typed, or null. Mirrors the agent. */
export function superStreamProblem(name: string, keys: string[], existing: string[]): string | null {
  if (!name) return 'Give it a name.'
  if (!NAME_RE.test(name)) return 'A name is letters, digits, . _ and -, up to 200.'
  if (name.startsWith('amq.')) return 'Names starting amq. are RabbitMQ\'s own.'
  if (keys.length === 0 || keys.length > MAX_SUPER_PARTITIONS) return `A super stream has 1 to ${MAX_SUPER_PARTITIONS} partitions.`
  for (const [i, k] of keys.entries()) {
    if (!KEY_RE.test(k)) return `"${k}" is not a routing key: letters, digits, . _ and -, up to 50.`
    if (keys.indexOf(k) !== i) return `"${k}" is named twice.`
  }
  if (existing.includes(name)) return `There is an exchange named ${name} already.`
  const taken = keys.map((k) => `${name}-${k}`).find((q) => existing.includes(q))
  if (taken) return `There is a queue named ${taken} already.`
  return null
}

/** What making a super stream runs, as the dialog previews it. */
export function superStreamPlan(name: string, keys: string[], maxAgeDays: number, maxGB: number): string[] {
  const keep = [maxAgeDays > 0 ? `x-max-age ${maxAgeDays}D` : '', maxGB > 0 ? `x-max-length-bytes ${maxGB} GB` : ''].filter(Boolean).join(', ')
  const lines = [`PUT exchange ${name}  direct`, '  x-super-stream true']
  const [k0] = keys
  if (k0 === undefined) return lines
  const first = `${name}-${k0}`
  const last = `${name}-${keys[keys.length - 1] ?? k0}`
  lines.push(`PUT queue ${keys.length > 1 ? `${first} … ${last}` : first}`, `  x-queue-type stream${keep ? `, ${keep}` : ''}`)
  lines.push(`POST binding ${name} → ${first}`, `  key ${k0}, x-super-stream-partition-order 0`)
  if (keys.length > 1) lines.push(`  … and ${keys.length - 1} more`)
  return lines
}
