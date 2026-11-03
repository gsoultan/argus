import { fireEvent, render, screen } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApplicationsOnServer, shortVersion } from '~/components/ApplicationsOnServer'
import { api } from '~/lib/api'
import { assets } from '~/lib/seed'
import { theme } from '~/theme'
import type { ServerApp, ServerApplications } from '~/types/domain'

/**
 * What runs on a server: a Postgres the agent found on the host is one step
 * from being a database, with its port filled in, and one inside a container
 * is not offered, because Argus could not open it.
 */

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  databasesOnAsset: async () => ({ databases: [], access: {} }),
  discoverDatabases: async () => ({ state: 'unsupported', databases: [] }),
}))

const app = (over: Partial<ServerApp>): ServerApp => ({
  kind: 'postgres', name: 'PostgreSQL', version: '', running: true, listen: [], process: '', unit: '',
  package: '', container: '', image: '', evidence: [], ...over,
})

const report: ServerApplications = {
  reported: true, checkedAt: '2026-11-02T05:00:00Z',
  applications: [
    app({ version: '16.4-1.pgdg24.04+1', package: 'postgresql-16',
      listen: [{ address: '127.0.0.1', port: 5433, localOnly: true }] }),
    app({ container: 'orders-pg', image: 'postgres:16', version: '16',
      listen: [{ address: '0.0.0.0', port: 15432, localOnly: false }] }),
    app({ kind: 'pgbouncer', name: 'PgBouncer', listen: [{ address: '127.0.0.1', port: 6432, localOnly: true }] }),
  ],
  listeners: [], containers: [], services: [], sources: ['sockets', 'dpkg'],
}

function renderCard(canEdit: boolean) {
  const asset = assets.find((a) => a.protocol !== 'rdp')!
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MantineProvider theme={theme}>
        <ApplicationsOnServer asset={asset} canEdit={canEdit} />
      </MantineProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.restoreAllMocks())

describe('ApplicationsOnServer', () => {
  it('offers the host Postgres with its port and PgBouncer filled in, and not the container', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue(report)
    renderCard(true)
    const buttons = await screen.findAllByRole('button', { name: /Add as database/ })
    expect(buttons).toHaveLength(1)
    expect(screen.getByText('in a container')).toBeTruthy()
    expect(screen.getAllByText('network')).toHaveLength(1)

    fireEvent.click(buttons[0]!)
    expect(((await screen.findByLabelText('Port')) as HTMLInputElement).value).toBe('5433')
    expect((screen.getByLabelText('PgBouncer port') as HTMLInputElement).value).toBe('6432')
  })

  it('offers a Redis and a RabbitMQ too, opening the form on their engine', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue({
      ...report,
      applications: [
        app({ kind: 'redis', name: 'Redis', unit: 'redis-server', listen: [{ address: '127.0.0.1', port: 6380, localOnly: true }] }),
        app({ kind: 'rabbitmq', name: 'RabbitMQ', unit: 'rabbitmq-server', listen: [
          { address: '0.0.0.0', port: 5672, localOnly: false }, { address: '0.0.0.0', port: 15672, localOnly: false }] }),
      ],
    })
    renderCard(true)
    fireEvent.click(await screen.findByRole('button', { name: /Add as database/ }))
    expect((screen.getByRole('radio', { name: 'Redis' }) as HTMLInputElement).checked).toBe(true)
    expect(((await screen.findByLabelText('Port')) as HTMLInputElement).value).toBe('6380')
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))

    fireEvent.click(await screen.findByRole('button', { name: /Add a vhost/ }))
    expect((screen.getByRole('radio', { name: 'RabbitMQ' }) as HTMLInputElement).checked).toBe(true)
    expect(((await screen.findByLabelText('Management port')) as HTMLInputElement).value).toBe('15672')
  })

  it('offers nothing to someone who cannot add databases', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue(report)
    renderCard(false)
    await screen.findAllByText('PostgreSQL')
    expect(screen.queryByRole('button', { name: /Add as database/ })).toBeNull()
  })

  it('says when the agent has not reported', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue({ ...report, reported: false, applications: [] })
    renderCard(true)
    expect(await screen.findByText('Not reported yet.')).toBeTruthy()
  })
})

describe('shortVersion', () => {
  it.each([
    ['5:7.0.15-1ubuntu0.24.04.5', '7.0.15'],
    ['16.4-1.pgdg24.04+1', '16.4'],
    ['1.24.0', '1.24.0'],
  ])('%s is %s', (full, short) => expect(shortVersion(full)).toBe(short))
})
