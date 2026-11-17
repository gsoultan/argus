import { fireEvent, render, screen, waitFor } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { describe, expect, it, vi } from 'vitest'
import { AddDatabase } from '~/components/DatabasesOnServer'
import { assets } from '~/lib/seed'
import { theme } from '~/theme'

/**
 * A server that runs only PgBouncer: when the agent cannot list the Postgres
 * PgBouncer sends to, a database typed by name is still added there, not on a
 * socket of this server, and the form says so.
 */

const created: Record<string, unknown>[] = []
vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  databasesOnAsset: async () => ({ databases: [], access: {} }),
  discoverDatabases: async () => ({
    state: 'failed', databases: [],
    error: 'no Postgres runs on this server, only PgBouncer (port 6432, /etc/pgbouncer/pgbouncer.ini), which sends its databases to Postgres on pg.example.id:5432 for *',
    poolers: [{ config: '/etc/pgbouncer/pgbouncer.ini', port: 6432, targets: [{ name: '*', host: 'pg.example.id', port: 5432 }] }],
  }),
  createDatabase: async (input: Record<string, unknown>) => { created.push(input); return { id: 'db1', ...input } },
}))

describe('AddDatabase behind PgBouncer', () => {
  it('adds a typed name on the Postgres PgBouncer sends to', async () => {
    const asset = assets.find((a) => a.protocol !== 'rdp')!
    render(
      <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
        <MantineProvider theme={theme}>
          <AddDatabase asset={asset} opened onClose={() => {}} initialEngine="postgres" initialPoolerPort={6432} />
        </MantineProvider>
      </QueryClientProvider>,
    )
    expect(await screen.findByText('Adding here', {}, { timeout: 3000 })).toBeTruthy()
    expect(screen.getByText(/As Postgres on pg\.example\.id knows it/)).toBeTruthy()
    fireEvent.change(screen.getByLabelText('Database name'), { target: { value: 'athena_db' } })
    fireEvent.click(screen.getByRole('button', { name: 'Add' }))
    await waitFor(() => expect(created).toHaveLength(1))
    expect(created[0]).toMatchObject({ dbname: 'athena_db', host: 'pg.example.id', port: 5432, poolerPort: 6432 })
  })
})
