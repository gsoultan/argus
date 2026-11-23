/*
Kafka topic and consumer group changes in the console's terms: the shapes the
agent answers with (internal/agent/kafkachange.go), a setting's value in
words and back, and what a dry run says. Pure.
*/

export interface KafkaSetting { key: string; name: string; kind: 'ms' | 'bytes' | 'int' | 'choice'; choices?: string[] }
export interface KafkaConfig { key: string; value: string; source: string; default: boolean; sensitive?: boolean; editable?: boolean }
export interface KafkaOffsetPlan { topic: string; partition: number; start: number; end: number; now: number; after: number }
export interface KafkaDryRun { plan: KafkaOffsetPlan[] | null; offsets: Record<string, Record<string, number>>; unread?: Record<string, number> }
export interface KafkaMember { id: string; client: string; host: string; topics?: string[] }
export interface KafkaGroupDetail { group: string; state: string; members: KafkaMember[]; offsets: KafkaOffsetPlan[]; outside: number }

export interface KafkaChange {
  set?: Record<string, string>
  reset?: string[]
  add?: number
  group?: string
  dryRun?: boolean
  to?: 'earliest' | 'latest' | 'time' | 'shift' | 'offset' | 'all'
  at?: number
  by?: number
  offset?: number
  partition?: number
  topics?: string[]
  offsets?: Record<string, Record<string, number>>
}

const MS = [
  { unit: 'days', ms: 86_400_000 }, { unit: 'hours', ms: 3_600_000 }, { unit: 'minutes', ms: 60_000 }, { unit: 'seconds', ms: 1000 },
] as const
const BYTES = [{ unit: 'GB', b: 1024 ** 3 }, { unit: 'MB', b: 1024 ** 2 }, { unit: 'KB', b: 1024 }] as const

/** A setting's value in words: "7 days", "1 GB", "no limit". */
export function settingWords(s: Pick<KafkaSetting, 'kind'>, v: string): string {
  if (v === '') return '–'
  const n = Number(v)
  if ((s.kind === 'ms' || s.kind === 'bytes') && n === -1) return 'no limit'
  if (s.kind === 'ms' && Number.isFinite(n)) {
    for (const u of MS) if (n >= u.ms && n % u.ms === 0) return `${n / u.ms} ${n / u.ms === 1 ? u.unit.slice(0, -1) : u.unit}`
    return `${n} ms`
  }
  if (s.kind === 'bytes' && Number.isFinite(n)) {
    for (const u of BYTES) if (n >= u.b && n % u.b === 0) return `${n / u.b} ${u.unit}`
    return `${n.toLocaleString()} bytes`
  }
  return v
}

export const MS_UNITS = MS.map((u) => u.unit)
export const BYTE_UNITS = [...BYTES.map((u) => u.unit), 'bytes'] as const

/** An amount in a unit as the value Kafka takes. */
export function toSettingValue(kind: KafkaSetting['kind'], amount: number, unit: string): string {
  if (kind === 'ms') return String(Math.round(amount * (MS.find((u) => u.unit === unit)?.ms ?? 1)))
  if (kind === 'bytes') return String(Math.round(amount * (BYTES.find((u) => u.unit === unit)?.b ?? 1)))
  return String(amount)
}

/** A value split into the largest whole unit, for the form. */
export function fromSettingValue(kind: KafkaSetting['kind'], v: string): { amount: number; unit: string } {
  const n = Number(v)
  if (kind === 'ms') {
    for (const u of MS) if (n >= u.ms && n % u.ms === 0) return { amount: n / u.ms, unit: u.unit }
    return { amount: n / 1000, unit: 'seconds' }
  }
  if (kind === 'bytes') {
    for (const u of BYTES) if (n >= u.b && n % u.b === 0) return { amount: n / u.b, unit: u.unit }
    return { amount: n, unit: 'bytes' }
  }
  return { amount: n, unit: '' }
}

/** How many messages a dry run moves past or deletes, in all. */
export function planCount(plan: KafkaOffsetPlan[] | null | undefined, kind: 'delete' | 'reset'): number {
  return (plan ?? []).reduce((n, p) => n + (kind === 'delete' ? p.after - p.now : p.after - Math.max(p.now, p.start)), 0)
}

/** How far behind a group would be on a partition after a reset. */
export const behindAfter = (p: KafkaOffsetPlan) => Math.max(0, p.end - p.after)
