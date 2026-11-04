import { describe, expect, it } from 'vitest'
import { alterTable, createTable, ddlQuery, designProblems, parseDesign, rowStatements, sqlValue, tableTemplate, type Design } from '~/lib/sqlGen'
import type { EditTarget } from '~/lib/rowEdit'
import type { Col } from '~/lib/resultView'

const orders: EditTarget = {
  table: 'public.orders', oid: 42, pk: [1],
  columns: [
    { attnum: 1, name: 'id', type: 'bigint', notNull: true, hasDefault: true, generated: false, identity: 'a' },
    { attnum: 2, name: 'note', type: 'text', notNull: false, hasDefault: false, generated: false, identity: '' },
    { attnum: 3, name: 'total', type: 'numeric(12,2)', notNull: false, hasDefault: false, generated: false, identity: '' },
    { attnum: 4, name: 'twice', type: 'numeric', notNull: false, hasDefault: true, generated: true, identity: '' },
    { attnum: 5, name: 'Paid?', type: 'boolean', notNull: false, hasDefault: false, generated: false, identity: '' },
  ],
}

describe('table templates', () => {
  it('takes values as :name parameters, keyed on the primary key', () => {
    expect(tableTemplate(orders, 'update')).toBe(
      'UPDATE public.orders\nSET note = :note,\n    total = :total,\n    "Paid?" = :p5\nWHERE id = :id\nRETURNING *;')
    expect(tableTemplate(orders, 'delete')).toBe('DELETE FROM public.orders\nWHERE id = :id\nRETURNING *;')
    expect(tableTemplate(orders, 'select')).toContain('SELECT id, note, total, twice, "Paid?"\nFROM public.orders\nWHERE id = :id')
  })

  it('leaves identity-always and generated columns out of an INSERT', () => {
    expect(tableTemplate(orders, 'insert')).toBe('INSERT INTO public.orders (note, total, "Paid?")\nVALUES (:note, :total, :p5)\nRETURNING *;')
    expect(tableTemplate(orders, 'upsert')).toContain('ON CONFLICT (id) DO UPDATE SET\n    note = EXCLUDED.note,')
  })

  it('names no rows when there is no primary key', () => {
    expect(tableTemplate({ ...orders, pk: [] }, 'delete')).toContain('WHERE false')
  })
})

describe('row statements', () => {
  const cols: Col[] = orders.columns.map((c) => ({ name: c.name, type: c.type, table: 42, attnum: c.attnum }))
  const rows = [['7', "it's", '12.50', '25', 't'], ['8', null, null, null, 'f']]

  it('writes each row with its values as literals', () => {
    expect(rowStatements(orders, cols, rows, 'update')).toBe(
      `UPDATE public.orders SET note = 'it''s', total = 12.50, "Paid?" = TRUE\nWHERE id = 7;\n\nUPDATE public.orders SET note = NULL, total = NULL, "Paid?" = FALSE\nWHERE id = 8;`)
    expect(rowStatements(orders, cols, rows.slice(0, 1), 'delete')).toBe('DELETE FROM public.orders WHERE id = 7;')
    expect(rowStatements(orders, cols, rows.slice(0, 1), 'insert')).toBe(
      `INSERT INTO public.orders (id, note, total, "Paid?") OVERRIDING SYSTEM VALUE\nVALUES (7, 'it''s', 12.50, TRUE);`)
    expect(rowStatements(orders, cols, rows.slice(0, 1), 'upsert')).toContain('ON CONFLICT (id) DO UPDATE SET note = EXCLUDED.note')
  })

  it('will not name a row without its key', () => {
    expect(rowStatements(orders, cols.slice(1), [rows[0]!.slice(1)], 'delete')).toMatch(/primary key is not in the result/)
  })

  it('writes numbers bare only when they are numbers', () => {
    expect(sqlValue('integer', '12')).toBe('12')
    expect(sqlValue('integer', '1; drop')).toBe("'1; drop'")
    expect(sqlValue('text', '12')).toBe("'12'")
  })
})

describe('the table designer', () => {
  const start: Design = parseDesign('public', 'orders', [
    ['id', 'bigint', 'true', 'GENERATED ALWAYS AS IDENTITY', 'orders_pkey', 'false', 'false'],
    ['note', 'text', 'false', "'x'::text", null, 'false', 'false'],
    ['old', 'integer', 'false', null, null, 'false', 'false'],
    ['twice', 'numeric', 'false', null, null, 'false', 'true'],
  ])

  it('reads a table, leaving generated columns alone', () => {
    expect(start.columns.map((c) => c.name)).toEqual(['id', 'note', 'old'])
    expect(start.pkName).toBe('orders_pkey')
    expect(alterTable(start, start)).toEqual([])
  })

  it('writes a CREATE TABLE', () => {
    const d: Design = { schema: 'sales', name: 'Line Items', columns: [
      { name: 'order_id', type: 'bigint', notNull: true, default: '', pk: true, unique: false },
      { name: 'n', type: 'integer', notNull: true, default: '', pk: true, unique: false },
      { name: 'sku', type: 'text', notNull: true, default: "'none'", pk: false, unique: true },
    ] }
    expect(createTable(d)).toBe(
      `CREATE TABLE sales."Line Items" (\n    order_id bigint,\n    n integer,\n    sku text DEFAULT 'none' NOT NULL UNIQUE,\n    PRIMARY KEY (order_id, n)\n);`)
  })

  it('writes the ALTER TABLE statements for what changed, renames first', () => {
    const to: Design = { ...start, columns: [
      start.columns[0]!,
      { ...start.columns[1]!, name: 'memo', type: 'varchar(200)', default: '', notNull: true },
      { name: 'paid', type: 'boolean', notNull: true, default: 'false', pk: false, unique: false },
    ] }
    expect(alterTable(start, to)).toEqual([
      'ALTER TABLE public.orders RENAME COLUMN note TO memo;',
      'ALTER TABLE public.orders DROP COLUMN old;',
      'ALTER TABLE public.orders ALTER COLUMN memo TYPE varchar(200) USING memo::varchar(200);',
      'ALTER TABLE public.orders ALTER COLUMN memo DROP DEFAULT;',
      'ALTER TABLE public.orders ALTER COLUMN memo SET NOT NULL;',
      'ALTER TABLE public.orders ADD COLUMN paid boolean DEFAULT false NOT NULL;',
    ])
  })

  it('moves the primary key by dropping and adding it', () => {
    const to: Design = { ...start, columns: [{ ...start.columns[0]!, pk: false }, { ...start.columns[1]!, pk: true }, start.columns[2]!] }
    const out = alterTable(start, to)
    expect(out[0]).toBe('ALTER TABLE public.orders DROP CONSTRAINT orders_pkey;')
    expect(out).toContain('ALTER TABLE public.orders ADD PRIMARY KEY (note);')
  })

  it('says what is wrong instead of writing broken SQL', () => {
    expect(designProblems({ schema: 'public', name: '', columns: [] })).toEqual(['The table needs a name.', 'A table needs at least one column.'])
    expect(designProblems({ ...start, columns: [start.columns[0]!, { ...start.columns[1]!, name: 'id' }] })).toContain('Two columns are called id.')
    expect(designProblems({ ...start, columns: [{ ...start.columns[1]!, default: "1); drop table x; --" }] })[0]).toMatch(/semicolon/)
  })
})

describe('DDL', () => {
  it('asks the database for the object kinds that have DDL', () => {
    expect(ddlQuery('table', 'public', 'Orders')?.params).toEqual(['public."Orders"'])
    expect(ddlQuery('function', 'public', 'f')?.params).toEqual(['public', 'f'])
    expect(ddlQuery('type', 'public', 'x')).toBeNull()
  })
})
