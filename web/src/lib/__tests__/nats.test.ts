import { describe, expect, it } from 'vitest'
import { isBroker } from '~/lib/engines'
import {
  appSubject, consumerProblems, fmtBytes, fmtNanos, natsAttention, parseBackoff, specOf, streamProblems, streamsFor,
  subjectMatches, subjectsOverlap, validSubject, type NatsConsumer, type NatsStream,
} from '~/lib/nats'

const stream = (name: string, subjects: string[], extra: Partial<NatsStream['config']> = {}, state: Partial<NatsStream['state']> = {}): NatsStream => ({
  config: { name, subjects, retention: 'limits', storage: 'file', num_replicas: 1, max_age: 0, max_bytes: -1, max_msgs: -1, max_msgs_per_subject: -1, discard: 'old', ...extra },
  created: '2026-11-01T00:00:00Z',
  state: { messages: 0, bytes: 0, first_seq: 0, first_ts: '', last_seq: 0, last_ts: '', consumer_count: 0, ...state },
})

describe('nats subjects', () => {
  it('knows a subject from a filter and from nonsense', () => {
    expect(['orders.eu.created', 'orders.*.created', 'orders.>', '>'].every(validSubject)).toBe(true)
    expect(['', 'orders..x', '.a', 'a.', 'a.>.b', 'a.b*', 'a b'].some(validSubject)).toBe(false)
  })
  it('lets apps publish only to literal subjects of their own', () => {
    expect(appSubject('orders.eu.created')).toBe(true)
    expect(['orders.*', 'orders.>', '$JS.API.INFO', '_INBOX.x'].some(appSubject)).toBe(false)
  })
  it('matches * as one token and > as the rest', () => {
    expect(subjectMatches('orders.>', 'orders.eu.created')).toBe(true)
    expect(subjectMatches('orders.>', 'orders')).toBe(false)
    expect(subjectMatches('orders.*', 'orders.eu.created')).toBe(false)
    expect(subjectMatches('orders.*.created', 'orders.us.created')).toBe(true)
  })
  it('sees when two filters can take the same subject', () => {
    expect(subjectsOverlap('orders.>', 'orders.eu.*')).toBe(true)
    expect(subjectsOverlap('orders.*', 'orders.eu.created')).toBe(false)
    expect(subjectsOverlap('orders.eu', 'payments.eu')).toBe(false)
    expect(subjectsOverlap('a.*', 'a.b')).toBe(true)
  })
  it('names the streams that store a subject', () => {
    expect(streamsFor('orders.eu.created', [stream('ORDERS', ['orders.>']), stream('PAY', ['payments.>'])])).toEqual(['ORDERS'])
  })
  it('puts NATS under Brokers', () => {
    expect(isBroker('nats')).toBe(true)
  })
})

describe('nats attention', () => {
  it('lists a stream without a leader, one near full, and a consumer nobody pulls', () => {
    const streams = [
      stream('ORDERS', ['orders.>'], { num_replicas: 3 }),
      stream('PAY', ['payments.>'], { max_msgs: 100, discard: 'new' }, { messages: 95 }),
    ]
    streams[0]!.cluster = { name: 'c1' }
    const stale = new Date(Date.now() - 3_600_000).toISOString()
    const consumers: NatsConsumer[] = [{
      stream_name: 'ORDERS', name: 'invoicer', created: stale, config: { deliver_policy: 'all', ack_policy: 'explicit' },
      delivered: { consumer_seq: 1, stream_seq: 1, last_active: stale }, ack_floor: { consumer_seq: 1, stream_seq: 1 },
      num_ack_pending: 0, num_redelivered: 0, num_waiting: 0, num_pending: 1234,
    }]
    const needs = natsAttention(null, streams, consumers)
    expect(needs.map((n) => [n.level, n.stream, n.consumer])).toEqual([['rose', 'ORDERS', undefined], ['amber', 'PAY', undefined], ['amber', 'ORDERS', 'invoicer']])
    expect(needs[1]!.text).toContain('refuses new messages')
    expect(needs[2]!.text).toContain('1,234 messages wait')
  })
  it('says nothing about a paused consumer', () => {
    const c = { stream_name: 'S', name: 'c', created: '2020-01-01T00:00:00Z', paused: true, config: { deliver_policy: 'all', ack_policy: 'explicit' as const },
      delivered: { consumer_seq: 0, stream_seq: 0 }, ack_floor: { consumer_seq: 0, stream_seq: 0 }, num_ack_pending: 0, num_redelivered: 0, num_waiting: 0, num_pending: 9 }
    expect(natsAttention(null, [], [c])).toEqual([])
  })
})

describe('nats forms', () => {
  it('turns a stream back into its form', () => {
    const s = specOf(stream('ORDERS', ['orders.>'], { max_age: 7 * 86_400 * 1e9, max_msgs: 1000, duplicate_window: 120e9, deny_delete: true }).config)
    expect(s).toMatchObject({ name: 'ORDERS', maxAgeS: 604_800, maxMsgs: 1000, maxBytes: 0, duplicatesS: 120, allowDelete: false })
  })
  it('refuses a stream on a subject another stream stores', () => {
    const p = streamProblems({ name: 'EU', subjects: ['orders.eu.*'] }, [stream('ORDERS', ['orders.>'])])
    expect(p).toEqual(['orders.eu.* overlaps orders.>, which ORDERS already stores. NATS refuses two streams on one subject.'])
    expect(streamProblems({ name: 'ORDERS', subjects: ['orders.>'] }, [stream('ORDERS', ['orders.>'])])).toEqual([])
    expect(streamProblems({ name: 'a.b', subjects: ['>'] }, [])).toHaveLength(2)
  })
  it('checks a consumer against its stream', () => {
    const s = stream('WORK', ['jobs.>'], { retention: 'workqueue' })
    expect(consumerProblems({ name: 'w', filterSubjects: ['orders.>'], ack: 'none' }, s)).toEqual([
      'WORK stores nothing on orders.>.', 'A work-queue stream takes only consumers that ack each message.',
    ])
    expect(consumerProblems({ name: 'w', deliver: 'by_start_sequence' })).toEqual(['Say the sequence to start at.'])
    expect(consumerProblems({ name: 'w', filterSubjects: ['jobs.email'], ack: 'explicit' }, s)).toEqual([])
  })
  it('reads backoff as seconds', () => {
    expect(parseBackoff('5, 30 120')).toEqual([5, 30, 120])
    expect(parseBackoff('')).toEqual([])
    expect(parseBackoff('5, soon')).toBeNull()
  })
  it('says sizes and durations as people do', () => {
    expect(fmtBytes(1536)).toBe('1.5 KB')
    expect(fmtNanos(0)).toBe('for ever')
    expect(fmtNanos(7 * 86_400e9)).toBe('7 days')
    expect(fmtNanos(30e9)).toBe('30 s')
  })
})
