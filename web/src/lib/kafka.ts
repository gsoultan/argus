/*
Kafka in the console's terms: the shapes the agent answers with for a scope
of topics, and the few rules the Kafka workspace applies to them -- which
topics a scope covers, what needs attention, and how to show a message. All
of it is pure, so it is tested without a cluster.
*/

export interface KafkaBroker { id: number; host: string; port: number; rack?: string; controller?: boolean }

export interface KafkaOverview {
  cluster: string
  brokers: KafkaBroker[]
  topics: number
  partitions: number
  scope: string
}

export interface KafkaTopic {
  name: string
  partitions: number
  replication: number
  messages: number
  underReplicated?: number
  offline?: number
  error?: string
}

export interface KafkaPartition { partition: number; leader: number; replicas: number[]; isr: number[]; start: number; end: number }

export interface KafkaGroup { group: string; state: string; members: number; lag?: Record<string, number>; error?: string }

export interface KafkaTopicDetail {
  name: string
  partitions: KafkaPartition[]
  configs: Record<string, string>
  groups: KafkaGroup[] | null
}

export interface KafkaMessage {
  partition: number
  offset: number
  time: string
  key?: string
  keyB64?: boolean
  value: string
  valueB64?: boolean
  cut?: boolean
  headers?: Record<string, string>
}

/** The topics a scope covers: every one for *, else those starting with it. Internal ones never. */
export function kafkaCovered(scope: string, topics: string[]): string[] {
  return topics.filter((t) => !t.startsWith('__') && (scope === '*' || t.startsWith(scope)))
}

/** What one entry's scope is, in words. */
export function scopeLabel(scope: string): string {
  return scope === '*' ? 'every topic' : `topics starting ${scope}`
}

/** A group's lag summed over the topics it reads. */
export function totalLag(g: KafkaGroup): number {
  return Object.values(g.lag ?? {}).reduce((a, b) => a + b, 0)
}

export type KafkaAttention = { level: 'rose' | 'amber'; text: string; topic?: string; group?: string }

/**
 * What an operator would want to know first: partitions without a leader,
 * partitions short of replicas, and groups that have stopped reading while
 * messages wait for them.
 */
export function kafkaAttention(topics: KafkaTopic[], groups: KafkaGroup[]): KafkaAttention[] {
  const out: KafkaAttention[] = []
  for (const t of topics) {
    if (t.offline) out.push({ level: 'rose', topic: t.name, text: `${t.name}: ${t.offline} partition${t.offline === 1 ? ' has' : 's have'} no leader, so nothing can be written to or read from ${t.offline === 1 ? 'it' : 'them'}.` })
    else if (t.underReplicated) out.push({ level: 'amber', topic: t.name, text: `${t.name}: ${t.underReplicated} partition${t.underReplicated === 1 ? ' is' : 's are'} short of in-sync replicas.` })
  }
  for (const g of groups) {
    const lag = totalLag(g)
    if (lag > 0 && (g.state === 'Empty' || g.members === 0)) {
      out.push({ level: 'amber', group: g.group, text: `${g.group} has no members while ${fmtCount(lag)} message${lag === 1 ? '' : 's'} wait for it.` })
    }
  }
  return out
}

export function fmtCount(n: number | undefined): string {
  if (n === undefined || !Number.isFinite(n)) return '–'
  if (n < 10_000) return n.toLocaleString()
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 100_000 ? 1 : 0)}k`
  if (n < 1_000_000_000) return `${(n / 1_000_000).toFixed(n < 10_000_000 ? 2 : 1)}M`
  return `${(n / 1_000_000_000).toFixed(2)}B`
}

/** A message value as it is best read: JSON indented, text as is, binary said so. */
export function showValue(m: Pick<KafkaMessage, 'value' | 'valueB64' | 'cut'>): { text: string; kind: 'json' | 'text' | 'base64' } {
  if (m.valueB64) return { text: m.value, kind: 'base64' }
  if (!m.cut) {
    const t = m.value.trim()
    if ((t.startsWith('{') && t.endsWith('}')) || (t.startsWith('[') && t.endsWith(']'))) {
      try { return { text: JSON.stringify(JSON.parse(t), null, 2), kind: 'json' } } catch { /* not JSON */ }
    }
  }
  return { text: m.value, kind: 'text' }
}

export function items<T>(d: unknown): T[] {
  return Array.isArray(d) ? (d as T[]) : []
}
