import { describe, expect, it } from 'vitest'
import { explain, keyWhere, needsWhy, tableSQL, type PGSubscriptionWhy } from '~/lib/pgSubscriptionWhy'

/* What the agent read from a real Postgres 16 log when athena_sub met a row
   it already had, with disable_on_error on. */
const dup: PGSubscriptionWhy = {
  name: 'athena_sub', enabled: false, disableOnError: 'on', binary: 'off', owner: 'rangga.rudiyanto', slot: 'athena_slot',
  origin: 'pg_16429', applyErrors: 4, syncErrors: 0, log: '/var/lib/postgresql/19/main/log/postgresql.json', errors: 4,
  turnedOff: true, turnedOffAt: '2026-11-28 09:56:22.434 UTC',
  error: {
    at: '2026-11-28 09:56:22.434 UTC', severity: 'ERROR', code: '23505', worker: 'apply',
    message: 'duplicate key value violates unique constraint "orders_pkey"', detail: 'Key (id)=(5) already exists.',
    context: 'processing remote data for replication origin "pg_16429" during message type "INSERT" for replication target relation "public.orders" in transaction 739, finished at 0/1D5B8F0',
    table: 'public.orders', change: 'INSERT', xid: '739', lsn: '0/1D5B8F0', constraint: 'orders_pkey', keyColumns: 'id', keyValues: '5',
  },
}

describe('why a subscription stopped', () => {
  it('names a duplicate row, its table and key, and the fix that loses nothing', () => {
    const x = explain(dup)
    expect(x.kind).toBe('duplicate')
    expect(x.headline).toMatch(/^athena_sub turned itself off after an error/)
    expect(x.cause).toContain('public.orders with key (id) = (5)')
    expect(x.fix).toMatch(/^Delete the copy here \(key id = 5\), then Enable/)
    expect(x.actions).toEqual(['sql', 'enable', 'skip'])
    expect(x.sql?.text).toContain('SELECT * FROM public.orders WHERE (id) = (\'5\');')
    expect(x.sql?.text).toContain('-- DELETE FROM public.orders WHERE (id) = (\'5\');')
  })

  it('gives each cause its own fix', () => {
    const missingCol = explain({ ...dup, name: 'autocount_sub', error: { severity: 'ERROR', message: 'logical replication target relation "public.items" is missing replicated column: "qty"', table: 'public.items', columns: '"qty"', lsn: '0/1D5C060' } })
    expect(missingCol.kind).toBe('missing-column')
    expect(missingCol.sql?.text).toContain('ALTER TABLE public.items ADD COLUMN qty')
    expect(missingCol.actions).not.toContain('skip') // nothing to lose: fix the column instead

    const denied = explain({ ...dup, error: { severity: 'ERROR', code: '42501', message: 'permission denied for table orders', table: 'orders' } })
    expect(denied.kind).toBe('permission')
    expect(denied.sql?.text).toContain('GRANT INSERT, UPDATE, DELETE, TRUNCATE ON orders TO "rangga.rudiyanto";')

    const conn = explain({ ...dup, error: { severity: 'ERROR', message: 'apply worker for subscription "athena_sub" could not connect to the publisher: connection to server at "10.0.2.9", port 6432 failed: FATAL:  password authentication failed for user "athena"' } })
    expect(conn.kind).toBe('connection')
    expect(conn.cause).toContain('refused the user name or password')
    expect(conn.actions[0]).toBe('connection')

    const binary = explain({ ...dup, binary: 'on', error: { severity: 'ERROR', code: '22P03', message: 'incorrect binary data format in logical replication column 2', table: 'public.orders' } })
    expect(binary.fix).toMatch(/Turn binary off/)

    const pg18 = explain({ ...dup, error: { severity: 'ERROR', message: 'conflict detected on relation "public.test": conflict=insert_exists', conflict: 'insert_exists', table: 'public.test', keyColumns: 'c', keyValues: '1', lsn: '0/14C0378' } })
    expect(pg18.kind).toBe('duplicate')

    const slot = explain({ ...dup, error: { severity: 'ERROR', message: 'could not receive data from WAL stream: ERROR:  replication slot "athena_slot" does not exist' } })
    expect(slot.fix).toContain("pg_create_logical_replication_slot('athena_slot', 'pgoutput')")
  })

  it('says what it could not read rather than guessing', () => {
    const blind = explain({ ...dup, error: undefined, log: undefined, logNote: 'the server does not log to a file Argus can read (logging_collector is off, log_destination is syslog).' })
    expect(blind.kind).toBe('unknown')
    expect(blind.cause).toContain('logging_collector is off')
    const counted = explain({ ...dup, error: undefined, log: undefined, logNote: 'x', conflicts: { insert_exists: 3 } })
    expect(counted.kind).toBe('duplicate')
    expect(counted.actions).not.toContain('skip') // no LSN without the log
  })

  it('says a subscription with disable_on_error off keeps retrying', () => {
    expect(explain({ ...dup, enabled: true, turnedOff: false, disableOnError: 'off' }).headline).toMatch(/keeps failing on the same change and retrying/)
  })

  it('quotes what goes into the editor', () => {
    expect(tableSQL('Sales.Order Items')).toBe('"Sales"."Order Items"')
    expect(keyWhere('a, b', "1, it's")).toBe("(a, b) = ('1', 'it''s')")
    expect(keyWhere('a, b', '1, x, y')).toBeNull() // a comma inside a value: no guess
  })

  it('asks why only for a subscription that is stopped with errors', () => {
    expect(needsWhy({ enabled: false, applyErrors: 4, syncErrors: 0 })).toBe(true)
    expect(needsWhy({ enabled: true, workerPid: 1, applyErrors: 4, syncErrors: 0 })).toBe(false)
    expect(needsWhy({ enabled: false, applyErrors: 0, syncErrors: 0 })).toBe(false)
  })
})
