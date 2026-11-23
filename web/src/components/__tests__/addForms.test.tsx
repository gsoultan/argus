import { fireEvent, render, screen, within } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AddDatabase, DatabasesOnServer } from '~/components/DatabasesOnServer'
import { api } from '~/lib/api'
import { assets } from '~/lib/seed'
import { databasesOnAssetQuery } from '~/lib/queries'
import { theme } from '~/theme'
import type { Database, ServerApp, ServerApplications } from '~/types/domain'

/**
 * Databases and brokers are added apart: Add database offers only database
 * engines, Add broker only brokers, each starting from what the agent found
 * running, and the server page keeps them in two cards.
 */

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  discoverDatabases: async () => ({ state: 'unsupported', databases: [] }),
}))

vi.mock('~/components/links', () => ({
  ButtonLink: ({ children }: { children: React.ReactNode }) => <a>{children}</a>,
}))

const app = (over: Partial<ServerApp>): ServerApp => ({
  kind: 'postgres', name: 'PostgreSQL', version: '16.4', running: true, listen: [{ address: '127.0.0.1', port: 5432, localOnly: true }],
  process: '', unit: '', package: '', container: '', image: '', evidence: [], ...over,
})
const report = (applications: ServerApp[]): ServerApplications => ({
  reported: true, checkedAt: '2026-11-23T05:00:00Z', applications, listeners: [], containers: [], services: [], sources: [],
})
const asset = assets.find((a) => a.protocol !== 'rdp')!

function mount(node: React.ReactNode, dbs: Database[] = []) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  qc.setQueryData(databasesOnAssetQuery(asset.id).queryKey, { databases: dbs, access: {} } as never)
  render(<QueryClientProvider client={qc}><MantineProvider theme={theme}>{node}</MantineProvider></QueryClientProvider>)
}

const radios = (group: string) =>
  within(screen.getByRole('radiogroup', { name: group })).getAllByRole('radio').map((r) => r.textContent ?? '')

afterEach(() => vi.restoreAllMocks())

describe('Add database and Add broker', () => {
  it('Add database offers database engines only, running ones first, and says what the agent found', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue(report([app({ kind: 'redis', name: 'Redis', version: '7.2.4',
      listen: [{ address: '127.0.0.1', port: 6379, localOnly: true }] })]))
    mount(<AddDatabase asset={asset} opened onClose={() => {}} kind="database" />)
    expect(await screen.findByText(/Running · 7\.2\.4 · port 6379/)).toBeTruthy()
    const offered = radios('Engine')
    expect(offered.map((t) => t.replace(/●.*|Not found.*|Asking.*/, '').trim())).toEqual(
      ['RdRedis / Valkey', 'PgPostgres', 'MyMySQL / MariaDB', 'MsSQL Server', 'MgMongoDB'])
    expect(offered.join(' ')).not.toMatch(/RabbitMQ|Kafka/)
  })

  it('Add broker offers RabbitMQ and Kafka only, in broker words', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue(report([]))
    mount(<AddDatabase asset={asset} opened onClose={() => {}} kind="broker" />)
    expect(await screen.findByText('Which broker?')).toBeTruthy()
    expect(radios('Engine').join(' ')).not.toMatch(/Postgres|MySQL|Redis/)
    expect(screen.getByText('Add a vhost that is already there')).toBeTruthy()
    fireEvent.click(screen.getByRole('radio', { name: /Kafka/ }))
    expect(screen.getByText('Give access to topics')).toBeTruthy()
    expect(screen.getByText('Create a new topic')).toBeTruthy()
  })

  it('Next shows the details with the choice on top, and Change goes back with it kept', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue(report([]))
    mount(<AddDatabase asset={asset} opened onClose={() => {}} kind="database" />)
    fireEvent.click(await screen.findByRole('radio', { name: /MongoDB/ }))
    fireEvent.click(screen.getByRole('button', { name: 'Next' }))
    expect(await screen.findByText(/^MongoDB on /)).toBeTruthy()
    // Where it listens is one line until asked for.
    expect(screen.getByText('Connects to')).toBeTruthy()
    expect(screen.queryByLabelText('Admin file')).toBeNull()
    fireEvent.click(screen.getAllByRole('button', { name: 'Change' }).at(-1)!)
    expect(screen.getByLabelText('Admin file')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Back' }))
    expect((screen.getByRole('radio', { name: /MongoDB/ })).getAttribute('aria-checked')).toBe('true')
  })

  it('opened with an engine, it starts on the details of the matching form', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue(report([]))
    mount(<AddDatabase asset={asset} opened onClose={() => {}} initialEngine="kafka" initialPort={9092} />)
    expect(await screen.findByText('Add broker')).toBeTruthy()
    expect(screen.getByText(/^Kafka on /)).toBeTruthy()
  })
})

describe('the server page', () => {
  const db = (id: string, dbname: string, engine: Database['engine']) =>
    ({ id, dbname, engine, assetId: asset.id, hostname: asset.hostname, host: '127.0.0.1', port: 1, label: 'production' }) as Database

  it('keeps databases and brokers in two cards, each with its own Add', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue(report([]))
    mount(<DatabasesOnServer asset={asset} canEdit />, [db('d1', 'billing', 'postgres'), db('d2', '/orders', 'rabbitmq')])
    expect(await screen.findByRole('button', { name: 'Add database' })).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Add broker' })).toBeTruthy()
    expect(screen.getByText('Brokers')).toBeTruthy()
  })

  it('shows no Brokers card when there is none and none runs here', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue(report([app({})]))
    mount(<DatabasesOnServer asset={asset} canEdit />, [db('d1', 'billing', 'postgres')])
    expect(await screen.findByRole('button', { name: 'Add database' })).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Add broker' })).toBeNull()
  })

  it('shows the Brokers card to an admin when a broker runs here', async () => {
    vi.spyOn(api, 'applications').mockResolvedValue(report([app({ kind: 'rabbitmq', name: 'RabbitMQ' })]))
    mount(<DatabasesOnServer asset={asset} canEdit />)
    expect(await screen.findByRole('button', { name: 'Add broker' })).toBeTruthy()
  })
})
