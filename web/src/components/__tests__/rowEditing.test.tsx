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
 * go. A cell the agent cut, the primary key, and a result that is not one
 * table's rows stay read only.
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

function setup(opts: { result?: GridResult; save?: (s: Stmt[]) => Promise<SaveResult>; cut?: Set<string> } = {}) {
  const collect = vi.fn(async (sql: string): Promise<Collected> => (sql === TARGET_SQL
    ? { cols: [], rows: TARGET_ROWS }
    : { cols: [], rows: [] }))
  const save = vi.fn(opts.save ?? (async (s: Stmt[]): Promise<SaveResult> => ({
    ok: true,
    returned: s.map(() => ({ cols: [{ name: 'id', type: 'integer', attnum: 1 }, { name: 'note', type: 'text', attnum: 2 }, { name: 'meta', type: 'jsonb', attnum: 3 }], rows: [['1', 'changed', '{"a":1}']] })),
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
    setup({ result: { ...result, cols: [...result.cols, { name: 'n', type: 'bigint' }], rows: result.rows.map((r) => [...r, '1']) } })
    expect(await screen.findByText(/Read only here: A column is computed/)).toBeInTheDocument()
    fireEvent.doubleClick(cell('first'))
    expect(screen.queryByLabelText('New value for note')).not.toBeInTheDocument()
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
