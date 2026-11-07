import { describe, expect, it } from 'vitest'
import { isBroker } from '~/lib/engines'
import { fmtCount, kafkaAttention, kafkaCovered, scopeLabel, showValue, totalLag } from '~/lib/kafka'

describe('kafka scope', () => {
  const topics = ['orders.eu', 'orders.us', 'payments', '__consumer_offsets']
  it('covers every topic for *, internal ones never', () => {
    expect(kafkaCovered('*', topics)).toEqual(['orders.eu', 'orders.us', 'payments'])
  })
  it('covers the topics a prefix starts', () => {
    expect(kafkaCovered('orders.', topics)).toEqual(['orders.eu', 'orders.us'])
    expect(kafkaCovered('ship', topics)).toEqual([])
  })
  it('says what a scope is', () => {
    expect(scopeLabel('*')).toBe('every topic')
    expect(scopeLabel('orders.')).toBe('topics starting orders.')
  })
  it('puts Kafka under Brokers with RabbitMQ', () => {
    expect(isBroker('kafka')).toBe(true)
    expect(isBroker('rabbitmq')).toBe(true)
    expect(isBroker('postgres')).toBe(false)
  })
})

describe('kafka attention', () => {
  it('lists offline partitions first, then short replicas, then stopped groups with lag', () => {
    const needs = kafkaAttention(
      [
        { name: 'a', partitions: 3, replication: 3, messages: 10, offline: 1 },
        { name: 'b', partitions: 3, replication: 3, messages: 10, underReplicated: 2 },
        { name: 'c', partitions: 1, replication: 1, messages: 0 },
      ],
      [
        { group: 'billing', state: 'Empty', members: 0, lag: { a: 1200, b: 34 } },
        { group: 'search', state: 'Stable', members: 2, lag: { a: 5 } },
        { group: 'idle', state: 'Empty', members: 0, lag: { a: 0 } },
      ],
    )
    expect(needs.map((n) => [n.level, n.topic ?? n.group])).toEqual([['rose', 'a'], ['amber', 'b'], ['amber', 'billing']])
    expect(needs[2]?.text).toContain('1,234 messages wait')
  })
  it('sums lag over topics', () => {
    expect(totalLag({ group: 'g', state: 'Stable', members: 1, lag: { a: 2, b: 3 } })).toBe(5)
    expect(totalLag({ group: 'g', state: 'Stable', members: 1 })).toBe(0)
  })
})

describe('kafka messages', () => {
  it('indents JSON, keeps text and says base64', () => {
    expect(showValue({ value: '{"a":1}' })).toEqual({ text: '{\n  "a": 1\n}', kind: 'json' })
    expect(showValue({ value: 'hello {' })).toEqual({ text: 'hello {', kind: 'text' })
    expect(showValue({ value: '/wAB', valueB64: true }).kind).toBe('base64')
    expect(showValue({ value: '{"a":', cut: true }).kind).toBe('text')
  })
  it('counts briefly', () => {
    expect(fmtCount(9999)).toBe('9,999')
    expect(fmtCount(12_345)).toBe('12.3k')
    expect(fmtCount(2_500_000)).toBe('2.50M')
  })
})
