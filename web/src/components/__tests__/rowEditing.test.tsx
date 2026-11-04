import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ResultsGrid, type GridResult } from '~/components/ResultsGrid'
import type { Collected } from '~/components/DatabaseAdmin'
import type { EditSupport, SaveResult } from '~/components/RowEdits'
import { TARGET_SQL, type Stmt } from '~/lib/rowEdit'
import { renderWithProviders } from '~/test/render'

/**
 * Editing rows from the results grid: a cell changes in place, nothing is
 * sent until Save, and Save hands the parameterized statements over in one
 * go. Each table of a join is edited on its own, a builder's JSON key is
 * written back into its document, and a cell the agent cut, the primary
 * key, and a computed column stay read only.
 */

const result: GridResult = {
  id: 1,
  sql: 'SELECT * FROM orders',
  cols: [
    { name: 'id', type: 'integer', table: 42, attnum: 1 },
    { name: 'note', type: 'text', table: 42, attnum: 2 },
    { name: 'meta', type: 'jsonb', table: 42, attnum: 3 },
  ],
  rows: [
    ['1', 'first', '{"a":1}'],
    ['2', 'second', null],
  ],
  done: { tag: 'SELECT 2', ms: 3 },
}

const TARGET_ROWS = [
  ['public.orders', '1', 'id', 'integer', 'true', 'true', 'false', null, '1'],
  ['public.orders', '2', 'note', 'text', 'false', 'false', 'false', null, '1'],
  ['public.orders', '3', 'meta', 'jsonb', 'false', 'false', 'false', null, '1'],
]

function setup(opts: {
  result?: GridResult; save?: (s: Stmt[]) => Promise<SaveResult>; cut?: Set<string>
  targets?: Record<number, (string | null)[][]>; returned?: (s: Stmt) => Collected
} = {}) {
  const targets = opts.targets ?? { 42: TARGET_ROWS }
  const collect = vi.fn(async (sql: string, _max?: number, params?: (string | null)[]): Promise<Collected> => (sql === TARGET_SQL
    ? { cols: [], rows: targets[Number(params?.[0])] ?? [] }
    : sql.startsWith('SELECT to_regclass') ? { cols: [], rows: [['42']] }
      : { cols: [], rows: [] }))
  const save = vi.fn(opts.save ?? (async (s: Stmt[]): Promise<SaveResult> => ({
    ok: true,
    returned: s.map((x) => opts.returned?.(x) ?? { cols: [{ name: 'id', type: 'integer', attnum: 1 }, { name: 'note', type: 'text', attnum: 2 }, { name: 'meta', type: 'jsonb', attnum: 3 }], rows: [['1', 'changed', '{"a":1}']] }),
  })))
  const onRows = vi.fn()
  const edit: EditSupport = { allowed: true, busy: false, inTransaction: false, cut: opts.cut, save }
  renderWithProviders(<ResultsGrid result={opts.result ?? result} narrow={false} collect={collect} edit={edit} onRows={onRows} />)
  return { collect, save, onRows }
}

const cell = (text: string) => screen.getByText(text).closest('td')!

describe('Editing rows in the results', () => {
  it('changes a cell, shows the SQL, and saves it as one batch', async () => {
    const { save, onRows, collect } = setup()
    expect(await screen.findByText(/double-click a cell to change it/)).toBeInTheDocument()
    expect(collect).toHaveBeenCalledWith(TARGET_SQL, 2000, ['42'])

    fireEvent.doubleClick(cell('first'))
    const input = screen.getByLabelText('New value for note')
    fireEvent.change(input, { target: { value: 'changed' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    expect(cell('changed').title).toBe('changed\nwas first')
    const bar = screen.getByRole('region', { name: 'Unsaved changes' })
    expect(within(bar).getByText('1 row changed')).toBeInTheDocument()

    fireEvent.click(within(bar).getByRole('button', { name: 'Review SQL' }))
    const dialog = await screen.findByRole('dialog')
    expect(dialog.textContent).toContain('SET note = CAST($1 AS text)')
    expect(dialog.textContent).toContain('$1 = "changed"')

    fireEvent.click(within(dialog).getByRole('button', { name: 'Save 1 change' }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    expect(save.mock.calls[0]![0]![0]!.params).toEqual(['changed', '1', 'first'])
    await waitFor(() => expect(onRows).toHaveBeenCalledWith([['1', 'changed', '{"a":1}'], ['2', 'second', null]]))
    await waitFor(() => expect(screen.queryByRole('region', { name: 'Unsaved changes' })).not.toBeInTheDocument())
  })

  it('keeps the changes and says why when a save fails', async () => {
    setup({ save: async () => ({ ok: false, at: 0, error: 'the row is gone, or someone changed it since you read it.' }) })
    await screen.findByText(/double-click a cell/)
    fireEvent.doubleClick(cell('second'))
    fireEvent.mouseDown(screen.getByRole('button', { name: 'Set to NULL' }))
    const bar = screen.getByRole('region', { name: 'Unsaved changes' })
    fireEvent.click(within(bar).getByRole('button', { name: 'Save' }))
    expect(await within(bar).findByText(/Nothing was saved\. Change 1 value in the row with id 2: the row is gone/)).toBeInTheDocument()
  })

  it('locks the primary key and cut cells', async () => {
    setup({ cut: new Set(['1:1']) })
    await screen.findByText(/double-click a cell/)
    fireEvent.doubleClick(screen.getAllByText('1').find((e) => e.tagName === 'TD')!)
    fireEvent.doubleClick(cell('second'))
    expect(screen.queryByRole('textbox', { name: /New value/ })).not.toBeInTheDocument()
  })

  it('says why a result is read only', async () => {
    setup({ result: { ...result, cols: [{ name: 'n', type: 'bigint' }], rows: result.rows.map(() => ['9']) } })
    expect(await screen.findByText(/Read only here: A column is computed/)).toBeInTheDocument()
    fireEvent.doubleClick(screen.getAllByText('9')[0]!.closest('td')!)
    expect(screen.queryByRole('textbox', { name: /New value/ })).not.toBeInTheDocument()
  })

  it('keeps a computed column read only and the rest editable', async () => {
    setup({ result: { ...result, cols: [...result.cols, { name: 'n', type: 'bigint' }], rows: result.rows.map((r) => [...r, '9']) } })
    expect(await screen.findByText(/some columns read only/)).toBeInTheDocument()
    fireEvent.doubleClick(screen.getAllByText('9')[0]!.closest('td')!)
    expect(screen.queryByRole('textbox', { name: /New value/ })).not.toBeInTheDocument()
    fireEvent.doubleClick(cell('first'))
    expect(screen.getByLabelText('New value for note')).toBeInTheDocument()
  })

  it('edits each table of a join, in one save', async () => {
    const joinResult: GridResult = {
      id: 2, sql: 'SELECT o.id, o.note, c.id, c.name FROM orders o JOIN customers c ON c.id = o.customer_id',
      cols: [
        { name: 'id', type: 'integer', table: 42, attnum: 1 }, { name: 'note', type: 'text', table: 42, attnum: 2 },
        { name: 'id', type: 'integer', table: 43, attnum: 1 }, { name: 'name', type: 'text', table: 43, attnum: 2 },
      ],
      rows: [['1', 'first', '5', 'Ana']],
      done: { tag: 'SELECT 1', ms: 1 },
    }
    const customers = [
      ['public.customers', '1', 'id', 'integer', 'true', 'true', 'false', null, '1'],
      ['public.customers', '2', 'name', 'text', 'true', 'false', 'false', null, '1'],
    ]
    const { save, onRows } = setup({ result: joinResult, targets: { 42: TARGET_ROWS, 43: customers }, returned: (s) => (s.sql.includes('customers')
      ? { cols: [{ name: 'id', type: 'integer', attnum: 1 }, { name: 'name', type: 'text', attnum: 2 }], rows: [['5', 'Ana Sari']] }
      : { cols: [{ name: 'id', type: 'integer', attnum: 1 }, { name: 'note', type: 'text', attnum: 2 }, { name: 'meta', type: 'jsonb', attnum: 3 }], rows: [['1', 'new note', null]] }) })
    expect(await screen.findByText(/public\.orders, public\.customers/)).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Open row 1' }))
    const panel = await screen.findByRole('dialog')
    fireEvent.click(within(panel).getByRole('button', { name: 'Edit' }))
    expect(within(panel).getByRole('group', { name: 'public.orders' })).toBeInTheDocument()
    const cust = within(panel).getByRole('group', { name: 'public.customers' })
    fireEvent.change(within(cust).getByLabelText('name'), { target: { value: 'Ana Sari' } })
    fireEvent.change(within(within(panel).getByRole('group', { name: 'public.orders' })).getByLabelText('note'), { target: { value: 'new note' } })

    const bar = screen.getByRole('region', { name: 'Unsaved changes' })
    fireEvent.click(within(bar).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    const [a, b] = save.mock.calls[0]![0]!
    expect(a!.sql).toContain('UPDATE public.orders\nSET note = CAST($1 AS text)\nWHERE id = CAST($2 AS integer)')
    expect(b!.sql).toContain('UPDATE public.customers\nSET name = CAST($1 AS text)\nWHERE id = CAST($2 AS integer)')
    expect(b!.params).toEqual(['Ana Sari', '5', 'Ana'])
    await waitFor(() => expect(onRows).toHaveBeenCalledWith([['1', 'new note', '5', 'Ana Sari']]))
  })

  it('writes a builder JSON key back into its document', async () => {
    const keyed: GridResult = {
      id: 3, sql: 'SELECT /* argus:json {"tier":["public.orders","meta",["tier"]]} */ o.id, o.meta->>\'tier\' AS tier FROM public.orders o',
      cols: [{ name: 'id', type: 'integer', table: 42, attnum: 1 }, { name: 'tier', type: 'text' }],
      rows: [['1', 'gold']],
      done: { tag: 'SELECT 1', ms: 1 },
    }
    const { save, collect } = setup({ result: keyed })
    await screen.findByText(/double-click a cell/)
    expect(collect).toHaveBeenCalledWith('SELECT to_regclass($1)::oid::text', 1, ['public.orders'])
    fireEvent.doubleClick(cell('gold'))
    const input = screen.getByLabelText('New value for tier')
    fireEvent.change(input, { target: { value: 'platinum' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    fireEvent.click(within(screen.getByRole('region', { name: 'Unsaved changes' })).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    const [u] = save.mock.calls[0]![0]!
    expect(u!.sql).toContain('SET meta = jsonb_set(COALESCE(meta::jsonb')
    expect(u!.params).toEqual(['{tier}', 'platinum', '1', '{tier}', 'gold'])
  })

  it('duplicates a row as a new one and deletes a row, with Undo', async () => {
    const { save } = setup()
    await screen.findByText(/double-click a cell/)
    fireEvent.click(screen.getByRole('button', { name: 'Open row 1' }))
    let panel = await screen.findByRole('dialog')
    fireEvent.click(within(panel).getByRole('button', { name: 'Duplicate as new row' }))
    panel = await screen.findByRole('dialog')
    expect(within(panel).getByText('New row in public.orders')).toBeInTheDocument()
    expect(within(panel).getByLabelText('note')).toHaveValue('first')
    fireEvent.click(within(panel).getByRole('button', { name: 'Done' }))

    fireEvent.click(screen.getByRole('button', { name: 'Open row 2' }))
    panel = await screen.findByRole('dialog')
    fireEvent.click(within(panel).getByRole('button', { name: 'Delete row' }))
    expect(within(panel).getByText(/deleted when you save/)).toBeInTheDocument()
    fireEvent.click(within(panel).getByRole('button', { name: 'Undo delete' }))
    fireEvent.click(within(panel).getByRole('button', { name: 'Delete row' }))

    const bar = screen.getByRole('region', { name: 'Unsaved changes' })
    expect(within(bar).getByText('1 new row · 1 row to delete')).toBeInTheDocument()
    fireEvent.click(within(bar).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    const [ins, del] = save.mock.calls[0]![0]!
    expect(ins!.sql).toBe('INSERT INTO public.orders (note, meta)\nVALUES (CAST($1 AS text), CAST($2 AS jsonb))\nRETURNING *')
    expect(ins!.params).toEqual(['first', '{"a":1}'])
    expect(del).toMatchObject({ sql: 'DELETE FROM public.orders\nWHERE id = CAST($1 AS integer)', params: ['2'], deletes: true })
  })

  it('edits jsonb as text and adds a new row', async () => {
    const { save } = setup()
    await screen.findByText(/double-click a cell/)
    fireEvent.doubleClick(screen.getAllByText('null')[0]!.closest('td')!)
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByText('Text'))
    const area = within(dialog).getByLabelText('JSON for meta')
    fireEvent.change(area, { target: { value: '{"tags": [' } })
    expect(within(dialog).getByRole('button', { name: 'Apply' })).toBeDisabled()
    fireEvent.change(area, { target: { value: '{"tags": ["x"]}' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Apply' }))

    fireEvent.click(screen.getByRole('button', { name: 'Add a row' }))
    const panel = await screen.findByRole('dialog')
    expect(within(panel).getByText('New row in public.orders')).toBeInTheDocument()
    fireEvent.click(within(panel).getByRole('button', { name: 'Set note' }))
    fireEvent.change(within(panel).getByLabelText('note'), { target: { value: 'hello' } })
    fireEvent.click(within(panel).getByRole('button', { name: 'Done' }))

    const bar = screen.getByRole('region', { name: 'Unsaved changes' })
    expect(within(bar).getByText('1 row changed · 1 new row')).toBeInTheDocument()
    fireEvent.click(within(bar).getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(save).toHaveBeenCalledTimes(1))
    const [upd, ins] = save.mock.calls[0]![0]!
    expect(upd!.params[0]).toBe('{"tags":["x"]}')
    expect(ins!.sql).toBe('INSERT INTO public.orders (note)\nVALUES (CAST($1 AS text))\nRETURNING *')
    expect(ins!.params).toEqual(['hello'])
  })
})
