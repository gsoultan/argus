import { describe, expect, it } from 'vitest'
import {
  attention, cameFrom, deadLetterQueues, headersMatch, rateSeries, routeKey, topicMatches, xDeath,
  type Binding, type Exchange, type Queue,
} from '~/lib/rabbit'

/**
 * The broker workspace's rules, against RabbitMQ's own semantics: which
 * queues a routing key reaches (the routing test and the publish preview both
 * rely on it), which queues hold dead letters, and what the overview flags.
 */

describe('topicMatches', () => {
  it.each([
    ['orders.*', 'orders.created', true],
    ['orders.*', 'orders.created.eu', false],
    ['orders.#', 'orders', true],
    ['orders.#', 'orders.created.eu', true],
    ['#', '', true],
    ['*.created.#', 'orders.created', true],
    ['*.created.#', 'created', false],
    ['orders.*.eu', 'orders.created.eu', true],
    ['orders.*.eu', 'orders.created.us', false],
    ['a.#.z', 'a.z', true],
    ['a.#.z', 'a.b.c.z', true],
    ['orders.created', 'orders.created', true],
  ])('%s against %s is %s', (p, k, want) => {
    expect(topicMatches(p, k)).toBe(want)
  })
})

describe('headersMatch', () => {
  it('needs every header for all, one for any, and ignores x- arguments', () => {
    const all = { 'x-match': 'all', tenant: 'acme', region: 'eu' }
    expect(headersMatch(all, { tenant: 'acme', region: 'eu' })).toBe(true)
    expect(headersMatch(all, { tenant: 'acme' })).toBe(false)
    expect(headersMatch({ ...all, 'x-match': 'any' }, { tenant: 'acme' })).toBe(true)
    expect(headersMatch({ 'x-match': 'all', 'x-other': 'q', tenant: 'acme' }, { tenant: 'acme' })).toBe(true)
  })
})

const q = (name: string, extra: Partial<Queue> = {}): Queue => ({ name, messages_ready: 0, consumers: 1, ...extra })
const b = (source: string, destination: string, routing_key: string, extra: Partial<Binding> = {}): Binding =>
  ({ source, destination, destination_type: 'queue', routing_key, ...extra })

describe('routeKey', () => {
  const exchanges: Exchange[] = [
    { name: 'orders', type: 'topic', arguments: { 'alternate-exchange': 'unrouted' } },
    { name: 'audit', type: 'fanout' },
    { name: 'unrouted', type: 'fanout' },
  ]
  const queues = [q('orders.created'), q('orders.eu-sync'), q('audit.all'), q('lost')]
  const bindings = [
    b('orders', 'orders.created', 'orders.created.*'),
    b('orders', 'orders.eu-sync', 'orders.*.eu'),
    b('orders', 'audit', 'orders.#', { destination_type: 'exchange' }),
    b('audit', 'audit.all', ''),
    b('unrouted', 'lost', ''),
  ]

  it('follows topic patterns and exchange-to-exchange bindings', () => {
    const r = routeKey('orders', 'orders.created.eu', {}, exchanges, bindings, queues)
    expect(r.map((x) => x.queue).sort()).toEqual(['audit.all', 'orders.created', 'orders.eu-sync'])
    expect(r.find((x) => x.queue === 'audit.all')!.via.map((v) => v.source)).toEqual(['orders', 'audit'])
  })

  it('sends what no binding takes to the alternate exchange', () => {
    expect(routeKey('orders', 'invoices.paid', {}, exchanges, bindings, queues).map((x) => x.queue)).toEqual(['lost'])
  })

  it('routes the default exchange by queue name, and only to a queue that exists', () => {
    expect(routeKey('', 'orders.created', {}, exchanges, bindings, queues).map((x) => x.queue)).toEqual(['orders.created'])
    expect(routeKey('', 'nope', {}, exchanges, bindings, queues)).toEqual([])
  })

  it('does not loop on exchanges bound to each other', () => {
    const ex: Exchange[] = [{ name: 'a', type: 'fanout' }, { name: 'b', type: 'fanout' }]
    const bs = [b('a', 'b', '', { destination_type: 'exchange' }), b('b', 'a', '', { destination_type: 'exchange' })]
    expect(routeKey('a', 'k', {}, ex, bs, [])).toEqual([])
  })
})

describe('dead letters', () => {
  it('finds dead-letter queues through the exchange they are bound to', () => {
    const queues = [q('orders.created', { arguments: { 'x-dead-letter-exchange': 'dlx.orders' } }), q('orders.parked'), q('jobs')]
    const dlq = deadLetterQueues(queues, [b('dlx.orders', 'orders.parked', '')])
    expect(dlq.get('orders.parked')).toBe('orders.created')
    expect(dlq.has('jobs')).toBe(false)
  })

  it('reads x-death and finds where most messages died', () => {
    const m = (queue: string) => ({ payload: '{}', properties: { headers: { 'x-death': [{ reason: 'rejected', queue, count: 3 }] } } })
    expect(xDeath(m('a').properties)[0]).toMatchObject({ reason: 'rejected', queue: 'a', count: 3 })
    expect(cameFrom([m('a'), m('b'), m('b')])).toBe('b')
    expect(cameFrom([{ payload: 'x' }])).toBeNull()
  })
})

describe('attention', () => {
  it('lists alarms, then queues without consumers, then dead letters, then unroutable', () => {
    const queues = [
      q('orders.created', { arguments: { 'x-dead-letter-exchange': '', 'x-dead-letter-routing-key': 'orders.failed' } }),
      q('orders.failed', { messages_ready: 12, consumers: 0 }),
      q('invoice.render', { messages_ready: 900, consumers: 0 }),
      q('idle', { messages_ready: 0, consumers: 0 }),
    ]
    const got = attention(queues, [], { message_stats: { drop_unroutable_details: { rate: 2 } } },
      [{ name: 'rabbit@mq-01', mem_alarm: true }])
    expect(got.map((a) => a.title)).toEqual([
      'Memory alarm on rabbit@mq-01',
      'invoice.render has no consumers',
      'orders.failed is collecting dead letters',
      'Messages are matching no binding',
    ])
  })
})

describe('rateSeries', () => {
  it('turns RabbitMQ’s cumulative samples, newest first, into per-second rates oldest first', () => {
    expect(rateSeries({ samples: [{ sample: 300, timestamp: 20_000 }, { sample: 100, timestamp: 10_000 }, { sample: 0, timestamp: 0 }] }))
      .toEqual([10, 20])
  })
})
