import { describe, expect, it } from 'vitest'
import { catalogFrom, completeSQL, quoteIdent, tableRefs, tokenize, type SQLCatalog, type SQLColumn } from '~/lib/sqlComplete'
import { completeRedis } from '~/lib/redisComplete'

const catalog: SQLCatalog = catalogFrom([
  { name: 'public', kind: 'path', detail: '1', schema: 'public' },
  { name: 'public', kind: 'schema', schema: 'public' },
  { name: 'billing', kind: 'schema', schema: 'billing' },
  { name: 'orders', kind: 'table', schema: 'public' },
  { name: 'order_items', kind: 'table', schema: 'public' },
  { name: 'customers', kind: 'table', schema: 'public' },
  { name: 'active_customers', kind: 'view', schema: 'public' },
  { name: 'invoices', kind: 'table', schema: 'billing' },
  { name: 'Orders Archive', kind: 'table', schema: 'billing' },
  { name: 'orders', kind: 'table', schema: 'billing' },
  { name: 'order_total', kind: 'function', detail: 'order_id bigint', schema: 'public' },
  { name: 'refund', kind: 'procedure', detail: 'invoice_id bigint', schema: 'billing' },
])

const cols: Record<string, SQLColumn[]> = {
  'public.orders': [{ name: 'id', type: 'bigint' }, { name: 'customer_id', type: 'bigint' }, { name: 'total', type: 'numeric' }, { name: 'created_at', type: 'timestamptz' }],
  'public.customers': [{ name: 'id', type: 'bigint' }, { name: 'email', type: 'text' }, { name: 'Full Name', type: 'text' }],
  'billing.invoices': [{ name: 'id', type: 'bigint' }, { name: 'amount', type: 'numeric' }],
}
const columnsOf = (s: string, n: string) => cols[`${s}.${n}`]

/** Completes at the `|` in `sql`. */
function at(sql: string, explicit = false, cat: SQLCatalog | null = catalog, cf = columnsOf) {
  const cursor = sql.indexOf('|')
  return completeSQL(sql.replace('|', ''), cursor, cat, cf, explicit)
}
const labels = (sql: string, explicit = false) => at(sql, explicit)?.items.map((i) => i.label) ?? []

describe('tokenize', () => {
  it('keeps strings, quoted names and comments whole', () => {
    const t = tokenize(`SELECT 'a;b', "My Col" -- x;\nFROM t /* ; */ WHERE $1 = $$;$$`)
    expect(t.filter((x) => x.k === 'punct' && x.v === ';')).toHaveLength(0)
    expect(t.find((x) => x.k === 'qident')?.v).toBe('My Col')
    expect(t.filter((x) => x.k === 'comment')).toHaveLength(2)
  })
})

describe('tableRefs', () => {
  it('reads tables, schemas and aliases from FROM lists and joins', () => {
    const refs = tableRefs(tokenize('SELECT * FROM orders o, billing.invoices AS i JOIN customers c ON c.id = o.customer_id WHERE 1=1'))
    expect(refs).toEqual([
      { name: 'orders', alias: 'o' },
      { schema: 'billing', name: 'invoices', alias: 'i' },
      { name: 'customers', alias: 'c' },
    ])
  })
  it('does not take a keyword for an alias', () => {
    expect(tableRefs(tokenize('SELECT 1 FROM orders WHERE true'))).toEqual([{ name: 'orders' }])
    expect(tableRefs(tokenize('UPDATE orders SET total = 0'))).toEqual([{ name: 'orders' }])
  })
})

describe('completeSQL', () => {
  it('offers keywords as typed, in the case typed', () => {
    expect(at('sel|')?.items[0]).toMatchObject({ label: 'SELECT', insert: 'select', kind: 'keyword' })
    expect(at('SEL|')?.items[0]?.insert).toBe('SELECT')
  })

  it('offers tables after FROM and JOIN, qualifying only what the search_path would not find', () => {
    const c = at('SELECT * FROM ord|')!
    expect(c.items.map((i) => i.insert)).toEqual(expect.arrayContaining(['orders', 'order_items', 'billing.orders']))
    expect(c.items.find((i) => i.kind === 'keyword')).toBeUndefined()
    expect(c.from).toBe('SELECT * FROM '.length)
    expect(at('SELECT * FROM orders o JOIN cus|')?.items[0]).toMatchObject({ insert: 'customers', kind: 'table' })
    // A name that needs quoting is quoted.
    expect(at('SELECT * FROM billing.Ord|')?.items.map((i) => i.insert)).toContain('"Orders Archive"')
  })

  it('offers a schema\'s tables after its name and a dot', () => {
    const c = at('SELECT * FROM billing.|')!
    expect(c.items.map((i) => i.insert)).toEqual(expect.arrayContaining(['invoices', 'orders', '"Orders Archive"']))
    expect(c.items.map((i) => i.label)).not.toContain('customers')
  })

  it('offers the columns of the tables a statement names, before it names them too', () => {
    expect(labels('SELECT cu| FROM orders')).toContain('customer_id')
    expect(labels('SELECT o.| FROM orders o JOIN customers c ON true')).toEqual(['id', 'customer_id', 'total', 'created_at'])
    expect(at('SELECT c.Fu| FROM orders o JOIN customers c ON true')?.items[0]?.insert).toBe('"Full Name"')
    expect(labels('SELECT * FROM billing.invoices WHERE am|')).toEqual(['amount'])
    expect(labels('SELECT public.orders.tot| FROM public.orders')).toEqual(['total'])
  })

  it('says which tables it needs columns for when it does not have them', () => {
    const c = at('SELECT o.| FROM orders o', false, catalog, () => undefined)
    expect(c?.items).toEqual([])
    expect(c?.needs).toEqual([{ schema: 'public', name: 'orders' }])
    // A table the role may not see is not asked for.
    expect(at('SELECT x.| FROM secret x', false, catalog, () => undefined)).toBeNull()
  })

  it('offers functions, user-defined and built-in, with an opening parenthesis', () => {
    expect(at('SELECT order_t|')?.items[0]).toMatchObject({ insert: 'order_total(', kind: 'function' })
    expect(at('SELECT coal|')?.items[0]).toMatchObject({ insert: 'coalesce(', kind: 'function' })
    // Off the search_path, a function is qualified.
    expect(at('CALL refu|')?.items[0]?.insert).toBe('billing.refund(')
  })

  it('stays quiet inside strings and comments, and with nothing typed unless asked', () => {
    expect(at("SELECT 'ord|'")).toBeNull()
    expect(at('SELECT 1 -- ord|')).toBeNull()
    expect(at('SELECT * FROM |')).toBeNull()
    expect(at('SELECT * FROM |', true)?.items.length).toBeGreaterThan(0)
  })

  it('reads only the statement the cursor is in', () => {
    expect(labels('SELECT * FROM customers; SELECT em| FROM orders')).not.toContain('email')
    expect(labels('SELECT * FROM orders; SELECT em| FROM customers')).toContain('email')
  })

  it('still offers keywords and built-ins with no catalogue (an older agent)', () => {
    expect(at('SELECT * FR|', false, null)?.items[0]?.label).toBe('FROM')
    expect(at('SELECT cou|', false, null)?.items[0]?.label).toBe('count')
  })

  it('quotes names Postgres would fold or refuse', () => {
    expect(quoteIdent('orders')).toBe('orders')
    expect(quoteIdent('Orders')).toBe('"Orders"')
    expect(quoteIdent('user')).toBe('"user"')
    expect(quoteIdent('a"b')).toBe('"a""b"')
  })
})

describe('completeRedis', () => {
  it('offers commands, then subcommands, then keys the browser has listed', () => {
    expect(completeRedis('hge', 3)?.items.map((i) => i.insert)).toEqual(['hget', 'hgetall'])
    expect(completeRedis('CONFIG G', 8)?.items.map((i) => i.label)).toEqual(['GET'])
    const keys = ['session:1', 'session:2', 'user:9']
    expect(completeRedis('GET sess', 8, keys)?.items.map((i) => i.insert)).toEqual(['session:1', 'session:2'])
    expect(completeRedis('MEMORY USAGE us', 15, keys)?.items.map((i) => i.insert)).toEqual(['user:9'])
    // Not where a command takes no key, and not inside a quoted argument.
    expect(completeRedis('PUBLISH sess', 12, keys)).toBeNull()
    expect(completeRedis('SET k "sess', 11, keys)).toBeNull()
  })
})
