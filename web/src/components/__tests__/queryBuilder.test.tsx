import { fireEvent, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { QueryBuilder } from '~/components/QueryBuilder'
import { ResultSummary } from '~/components/ResultSummary'
import { renderWithProviders } from '~/test/render'
import { COLUMNS_SQL, FKS_SQL, TABLES_SQL } from '~/lib/queryBuilder'
import type { Collected } from '~/components/DatabaseAdmin'

/** The visual query builder writes a SELECT from the catalogue; Summarize charts a result. */

const ok = (rows: (string | null)[][]): Collected => ({ cols: [], rows })

function catalogue() {
  return vi.fn(async (sql: string, _max?: number, params?: (string | null)[]): Promise<Collected> => {
    if (sql === TABLES_SQL) return ok([['public', 'customers', 'r'], ['public', 'orders', 'r']])
    if (sql === COLUMNS_SQL) return ok(params?.[0] === 'public.orders'
      ? [['id', 'bigint'], ['customer_id', 'bigint'], ['meta', 'jsonb']]
      : [['id', 'bigint'], ['name', 'text']])
    if (sql === FKS_SQL) return ok([['orders_customer_fk', 'public', 'orders', '["customer_id"]', 'public', 'customers', '["id"]']])
    if (sql.includes('jsonb_object_keys')) return ok([['tier', '2']])
    return { cols: [], rows: [], error: { error: 'unexpected' } }
  })
}

describe('Build a query', () => {
  it('joins along a foreign key, finds JSON keys, and opens the SELECT', async () => {
    const collect = catalogue()
    const onOpen = vi.fn()
    renderWithProviders(<QueryBuilder collect={collect} start={{ schema: 'public', name: 'orders' }} narrow={false} onClose={vi.fn()} onOpen={onOpen} />)
    expect(await screen.findByLabelText('The query')).toHaveTextContent('SELECT t0.* FROM public.orders t0 LIMIT 100;')

    fireEvent.click(await screen.findByRole('button', { name: 'Find keys in meta' }))
    expect(await screen.findByRole('button', { name: 'meta: 1 keys' })).toBeDisabled()
    expect(collect).toHaveBeenCalledWith(expect.stringContaining('SELECT meta::jsonb AS j FROM public.orders'), 200)

    fireEvent.click(screen.getByRole('button', { name: 'Join a related table' }))
    fireEvent.click(await screen.findByRole('menuitem', { name: /public\.customers/ }))
    await waitFor(() => expect(screen.getByLabelText('The query')).toHaveTextContent('JOIN public.customers t1 ON t1.id = t0.customer_id'))

    fireEvent.click(screen.getByRole('button', { name: 'Add a filter' }))
    expect(screen.getByLabelText('The query')).toHaveTextContent('WHERE t0.id = :id')
    fireEvent.click(screen.getByRole('button', { name: 'Add a count or total' }))
    expect(screen.getByLabelText('The query')).toHaveTextContent('count(*) AS count')

    fireEvent.click(screen.getByRole('button', { name: 'Open in a new tab' }))
    expect(onOpen).toHaveBeenCalledWith(expect.stringContaining('FROM public.orders t0\nJOIN public.customers t1'), 'query orders')
  })
})

describe('Summarize', () => {
  const cols = [{ name: 'status', type: 'text' }, { name: 'total', type: 'numeric' }, { name: 'meta', type: 'jsonb' }]
  const rows = [['paid', '10', '{"tier":"gold"}'], ['paid', '4', '{"tier":"silver"}'], ['open', '1', '{"tier":"gold"}']]

  it('charts a count, shows it as a table, and writes the GROUP BY for every row', () => {
    const onRunSQL = vi.fn()
    renderWithProviders(<ResultSummary cols={cols} rows={rows} sql="SELECT * FROM orders" truncated onRunSQL={onRunSQL} />)
    expect(screen.getByRole('img', { name: 'Rows by status' })).toBeInTheDocument()
    expect(screen.getByText(/summarizes the 3 rows loaded/)).toBeInTheDocument()
    fireEvent.click(screen.getByText('Table'))
    const table = screen.getByRole('table', { name: 'Summary' })
    expect(table).toHaveTextContent('paid2')
    fireEvent.click(screen.getByRole('button', { name: 'Summarize every row in the database' }))
    expect(onRunSQL).toHaveBeenCalledWith(expect.stringContaining('SELECT q.status AS status, count(*) AS count'))
  })
})
