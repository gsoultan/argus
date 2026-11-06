import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ResultsGrid, type GridResult } from '~/components/ResultsGrid'
import type { Collected } from '~/components/DatabaseAdmin'
import { renderWithProviders } from '~/test/render'

/**
 * The results grid works on the rows it has: sort, filter, Copy as and the
 * row panel run nothing. A foreign key value opens the row it points at,
 * read with a bound parameter.
 */

const result: GridResult = {
  id: 1,
  sql: 'SELECT * FROM orders',
  cols: [
    { name: 'id', type: 'integer', table: 42, attnum: 1 },
    { name: 'customer_id', type: 'integer', table: 42, attnum: 2 },
    { name: 'meta', type: 'jsonb', table: 42, attnum: 3 },
  ],
  rows: [
    ['1', '7', '{"tags":["a","b"],"n":1}'],
    ['2', '8', null],
    ['3', '7', '[1,2,3]'],
  ],
  done: { tag: 'SELECT 3', ms: 4 },
}

const rowNumbers = () => screen.getAllByRole('button', { name: /^Open row \d+$/ }).map((b) => b.textContent)

function setup(collect?: (sql: string, max?: number, params?: (string | null)[]) => Promise<Collected>) {
  const onAdd = vi.fn()
  renderWithProviders(<ResultsGrid result={result} narrow={false} collect={collect} onAddToQuery={onAdd} />)
  return onAdd
}

describe('Results grid', () => {
  it('sorts and filters the loaded rows, and hands the same thing to the query as SQL', async () => {
    const onAdd = setup()
    expect(rowNumbers()).toEqual(['1', '2', '3'])
    fireEvent.click(screen.getByRole('button', { name: 'Column id' }))
    fireEvent.click(await screen.findByRole('menuitem', { name: 'Sort descending' }))
    await waitFor(() => expect(rowNumbers()).toEqual(['3', '2', '1']))

    fireEvent.click(screen.getByRole('button', { name: 'Column customer_id' }))
    const input = await screen.findByLabelText('Filter customer_id')
    fireEvent.change(input, { target: { value: '7' } })
    fireEvent.keyDown(input, { key: 'Enter' })
    await waitFor(() => expect(rowNumbers()).toEqual(['3', '1']))
    expect(screen.getByText('2 of 3 loaded rows')).toBeInTheDocument()

    fireEvent.click(screen.getByRole('button', { name: 'Add to the query' }))
    expect(onAdd).toHaveBeenCalledWith(
      "SELECT *\nFROM (\nSELECT * FROM orders\n) q\nWHERE q.customer_id::text ILIKE '%' || :f_customer_id || '%'\nORDER BY q.id DESC NULLS LAST",
      { f_customer_id: '7' })
  })

  it('shows every field of a row, JSON as a tree or laid-out text, and steps through rows', async () => {
    setup()
    fireEvent.click(screen.getByRole('button', { name: 'Open row 1' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Row 1')).toBeInTheDocument()
    fireEvent.click(await within(dialog).findByRole('button', { name: 'Open tags' }))
    expect(within(dialog).getByText('b')).toBeInTheDocument()
    fireEvent.click(within(dialog).getByText('Text'))
    expect(dialog.textContent).toContain('"tags": [\n    "a",')
    fireEvent.click(within(dialog).getByRole('button', { name: 'Next row' }))
    await waitFor(() => expect(within(dialog).getByText('Row 2')).toBeInTheDocument())
    expect(within(dialog).getByText('NULL')).toBeInTheDocument()
  })

  it('copies the rows as shown', async () => {
    const writeText = vi.fn().mockResolvedValue(undefined)
    Object.defineProperty(navigator, 'clipboard', { value: { writeText }, configurable: true })
    setup()
    fireEvent.click(screen.getByRole('button', { name: /Copy as/ }))
    fireEvent.click(await screen.findByRole('menuitem', { name: 'INSERT statements' }))
    await waitFor(() => expect(writeText).toHaveBeenCalled())
    expect(writeText.mock.calls[0]![0]).toBe(
      `INSERT INTO orders (id, customer_id, meta) VALUES (1, 7, '{"tags":["a","b"],"n":1}');\n`
      + 'INSERT INTO orders (id, customer_id, meta) VALUES (2, 8, NULL);\n'
      + `INSERT INTO orders (id, customer_id, meta) VALUES (3, 7, '[1,2,3]');`)
  })

  it('opens the row a foreign key points at, with the value as a parameter', async () => {
    const collect = vi.fn(async (sql: string, _max?: number, params?: (string | null)[]): Promise<Collected> => {
      if (sql.includes('pg_constraint')) {
        expect(params).toEqual(['{42}'])
        return { cols: [], rows: [['42', '2', 'public.customers', 'id']] }
      }
      return { cols: [{ name: 'id', type: 'integer' }, { name: 'email', type: 'text' }], rows: [[params?.[0] ?? null, 'ana@example.com']] }
    })
    setup(collect)
    const link = (await screen.findAllByRole('button', { name: 'Open public.customers row 7' }))[0]!
    fireEvent.click(link)
    const dialog = await screen.findByRole('dialog')
    await within(dialog).findByText('ana@example.com')
    expect(collect).toHaveBeenLastCalledWith('SELECT * FROM public.customers WHERE id = $1 LIMIT 1', 1, ['7'])
    fireEvent.click(within(dialog).getByRole('button', { name: 'Back' }))
    await waitFor(() => expect(within(dialog).getByText('Row 1')).toBeInTheDocument())
  })

  it('marks a value the agent cut at its size limit, in the grid and the row panel', async () => {
    renderWithProviders(<ResultsGrid result={{ ...result, cut: new Set(['0:2']) }} narrow={false} />)
    expect(screen.getAllByText('cut')).toHaveLength(1)
    fireEvent.click(screen.getByRole('button', { name: 'Open row 1' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('cut')).toBeInTheDocument()
    expect(within(dialog).getByText(/loaded only the first part of this value/)).toBeInTheDocument()
  })

  it('says what a permission error means at the session\'s level, and offers more access only where it helps', () => {
    const refused: GridResult = { id: 2, sql: 'ALTER TABLE quotations ADD c int', cols: [], rows: [],
      error: { error: 'must be owner of table quotations', code: '42501' } }
    const { unmount } = renderWithProviders(<ResultsGrid result={refused} narrow={false} level="owner" canAsk />)
    expect(screen.getByText(/You already have OWNER access, so asking for more will not help/)).toBeInTheDocument()
    expect(screen.queryByText(/Ask for write access/)).not.toBeInTheDocument()
    unmount()
    renderWithProviders(<ResultsGrid result={refused} narrow={false} level="read" canAsk />)
    expect(screen.getByText(/Use Ask for write access above/)).toBeInTheDocument()
  })
})
