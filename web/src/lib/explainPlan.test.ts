import { describe, expect, it } from 'vitest'
import { explainSQL, explainable, parsePlan } from './explainPlan'

// Postgres 16's own output for a query with a slow scan and a bad estimate.
const ANALYZED = `Sort  (cost=1887.48..1887.65 rows=67 width=48) (actual time=16.983..16.988 rows=0 loops=1)
  Sort Key: o.created_at DESC
  Sort Method: quicksort  Memory: 25kB
  Buffers: shared hit=588
  ->  Nested Loop  (cost=1141.69..1885.45 rows=67 width=48) (actual time=16.970..16.975 rows=0 loops=1)
        Buffers: shared hit=585
        ->  Hash Right Join  (cost=1141.42..1865.26 rows=67 width=24) (actual time=16.969..16.973 rows=0 loops=1)
              Hash Cond: (s.order_id = o.id)
              Filter: (s.id IS NULL)
              Rows Removed by Filter: 10079
              Buffers: shared hit=585
              ->  Seq Scan on shipments s  (cost=0.00..618.45 rows=40145 width=16) (actual time=0.009..3.642 rows=40000 loops=1)
                    Buffers: shared hit=217
              ->  Hash  (cost=992.68..992.68 rows=11899 width=24) (actual time=8.197..8.199 rows=10079 loops=1)
                    Buckets: 16384  Batches: 1  Memory Usage: 680kB
                    Buffers: shared hit=368
                    ->  Seq Scan on orders o  (cost=0.00..992.68 rows=11899 width=24) (actual time=0.009..6.817 rows=10079 loops=1)
                          Filter: (created_at > (now() - '7 days'::interval))
                          Rows Removed by Filter: 39921
                          Buffers: shared hit=368
        ->  Index Scan using customers_pkey on customers c  (cost=0.28..0.30 rows=1 width=40) (never executed)
              Index Cond: (id = o.customer_id)
Planning:
  Buffers: shared hit=69
Planning Time: 0.314 ms
Execution Time: 17.026 ms`.split('\n')

describe('parsePlan', () => {
  it('builds the tree with times', () => {
    const p = parsePlan(ANALYZED)
    expect(p.analyzed).toBe(true)
    expect(p.executionMs).toBeCloseTo(17.026)
    expect(p.planningMs).toBeCloseTo(0.314)
    expect(p.nodes.map((n) => [n.type, n.depth])).toEqual([
      ['Sort', 0], ['Nested Loop', 1], ['Hash Right Join', 2], ['Seq Scan', 3], ['Hash', 3], ['Seq Scan', 4], ['Index Scan', 2],
    ])
    const orders = p.nodes[5]!
    expect(orders.relation).toBe('orders')
    expect(orders.target).toBe('on orders o')
    expect(orders.details).toContain('Rows Removed by Filter: 39921')
    expect(p.nodes[6]!.neverExecuted).toBe(true)
    expect(p.nodes[6]!.target).toBe('using customers_pkey on customers c')
    // The planning buffers are not the last node's.
    expect(p.nodes[6]!.details).toEqual(['Index Cond: (id = o.customer_id)'])
    // Self time takes the children out: the hash is almost all its scan.
    expect(p.nodes[4]!.selfMs!).toBeLessThan(1.5)
  })

  it('finds the scan that throws most rows away', () => {
    const p = parsePlan(ANALYZED)
    const scan = p.findings.find((f) => f.title.startsWith('Seq Scan on orders'))
    expect(scan).toBeDefined()
    expect(scan!.title).toContain('reads 50k rows to keep 10k')
    expect(scan!.detail).toContain("created_at > (now() - '7 days'::interval)")
  })

  it('flags a bad estimate and offers ANALYZE', () => {
    const p = parsePlan([
      'Hash Join  (cost=1.00..2.00 rows=3 width=8) (actual time=0.1..1262.0 rows=214 loops=1)',
      '  ->  Seq Scan on shipments s  (cost=0.00..1.00 rows=10 width=8) (actual time=0.1..1108.0 rows=1902113 loops=1)',
    ])
    const est = p.findings.find((f) => f.title.startsWith('Row estimate'))
    expect(est?.title).toMatch(/Row estimate \d+× too low/)
    expect(p.findings.some((f) => f.fix === 'ANALYZE shipments;')).toBe(true)
  })

  it('reads a plan without ANALYZE', () => {
    const p = parsePlan(['Seq Scan on orders  (cost=0.00..992.68 rows=50000 width=24)'])
    expect(p.analyzed).toBe(false)
    expect(p.root?.estRows).toBe(50000)
    expect(p.root?.totalMs).toBeUndefined()
  })

  it('flags a sort that spilled', () => {
    const p = parsePlan([
      'Sort  (cost=1..2 rows=10 width=8) (actual time=1..900 rows=1000000 loops=1)',
      '  Sort Method: external merge  Disk: 18432kB',
    ])
    expect(p.findings.some((f) => f.title === 'Sort spilled to disk')).toBe(true)
  })
})

describe('findings', () => {
  it('reports a misestimate once, on the scan it starts at', () => {
    const plan = parsePlan(`Hash Join  (cost=1.00..9800.00 rows=120 width=64) (actual time=0.5..412.3 rows=48210 loops=1)
  Hash Cond: (o.customer_id = c.id)
  ->  Seq Scan on orders o  (cost=0.00..8000.00 rows=100 width=40) (actual time=0.01..380.20 rows=48000 loops=1)
        Filter: (status = 'paid'::text)
        Rows Removed by Filter: 952000
  ->  Hash  (cost=1.00..1.00 rows=50 width=24) (actual time=0.2..0.2 rows=50 loops=1)
        ->  Seq Scan on customers c  (cost=0.00..1.00 rows=50 width=24) (actual time=0.01..0.05 rows=50 loops=1)
Execution Time: 415.1 ms`.split('\n'))
    const estimates = plan.findings.filter((f) => f.title.startsWith('Row estimate'))
    expect(estimates).toHaveLength(1)
    expect(estimates[0]!.node.type).toBe('Seq Scan')
    expect(estimates[0]!.fix).toBe('ANALYZE orders;')
  })
})

describe('explainSQL', () => {
  it('drops the trailing semicolon and adds the options', () => {
    expect(explainSQL('select 1;\n', 'plan')).toBe('EXPLAIN select 1')
    expect(explainSQL('select 1', 'buffers')).toBe('EXPLAIN (ANALYZE, BUFFERS) select 1')
  })
  it('knows what EXPLAIN takes', () => {
    expect(explainable('-- note\nSELECT 1')).toBe(true)
    expect(explainable('update t set a = 1')).toBe(true)
    expect(explainable('vacuum t')).toBe(false)
    expect(explainable('drop table t')).toBe(false)
  })
})
