import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { GiveAccessMany, countKinds, whoHasIt } from '~/components/BulkDBAccess'
import { teamsQuery, usersQuery } from '~/lib/queries'
import { theme } from '~/theme'
import type { BulkAccessLine, DBAssignment, Database } from '~/types/domain'

/**
 * Giving many databases at once: the dialog shows what the control plane
 * says would change before anything is written, keeps anyone who has more
 * unless told otherwise, and uses broker words for brokers.
 */

const api = vi.hoisted(() => ({ planDBAccess: vi.fn(), giveDBAccess: vi.fn() }))
vi.mock('~/lib/dbAccessApi', () => api)
vi.mock('~/lib/live', async (orig) => ({ ...(await orig<typeof import('~/lib/live')>()), isConfigured: () => true }))

const db = (id: string, dbname: string, engine: Database['engine'] = 'postgres', assetId = 'a1') =>
  ({ id, dbname, engine, assetId, hostname: 'db-01.impack.local', label: 'production' }) as Database

const line = (over: Partial<BulkAccessLine>): BulkAccessLine => ({
  databaseId: 'd1', databaseName: 'billing', engine: 'postgres', hostname: 'db-01.impack.local', label: 'production',
  teamId: 'team1', teamName: 'Payments', before: '', beforeApproval: false, after: 'write', afterApproval: true, change: 'new', ...over,
})

function mount(targets: Database[]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } })
  qc.setQueryData(teamsQuery().queryKey, [{ id: 'team1', name: 'Payments', members: ['a@x'], servers: [], createdBy: '', createdAt: '' }])
  qc.setQueryData(usersQuery().queryKey, [{ id: 'u', email: 'rina@x', displayName: 'Rina', role: 'operator', mfaEnrolled: true, lastSeenAt: null }])
  render(
    <QueryClientProvider client={qc}><MantineProvider theme={theme}>
      <GiveAccessMany opened targets={targets} initialWho={['t:team1', 'u:rina@x']} onClose={() => {}} />
    </MantineProvider></QueryClientProvider>,
  )
  return screen.findByRole('dialog')
}

afterEach(() => vi.clearAllMocks())

describe('GiveAccessMany', () => {
  it('previews, keeps who has more, and gives on every one picked', async () => {
    api.planDBAccess.mockResolvedValue({ lines: [
      line({}), line({ databaseId: 'd2', databaseName: 'orders', change: 'same', before: 'write', beforeApproval: true }),
      line({ teamId: undefined, teamName: undefined, userEmail: 'rina@x', before: 'owner', after: 'owner', change: 'kept' }),
    ] })
    api.giveDBAccess.mockResolvedValue({ lines: [line({})], rules: null })
    const dialog = await mount([db('d1', 'billing'), db('d2', 'orders'), db('v1', '/orders', 'rabbitmq')])

    // Databases and brokers each get their own words.
    expect(within(dialog).getByText('Databases · what they can do')).toBeTruthy()
    expect(within(dialog).getAllByText('Manage').length).toBeGreaterThan(0)
    expect(await within(dialog).findByText('1 has more, kept')).toBeTruthy()
    expect(within(dialog).getByText('1 already has it')).toBeTruthy()
    expect(within(dialog).getByLabelText('Also lower anyone who has more')).toBeTruthy()
    // Everything is on one server, so the ones added later can be given too.
    expect(within(dialog).getByLabelText('Also give it on databases and brokers added to db-01 later')).toBeTruthy()

    fireEvent.click(within(dialog).getByRole('button', { name: 'Give access on 3' }))
    await waitFor(() => expect(api.giveDBAccess).toHaveBeenCalled())
    const sent = api.giveDBAccess.mock.calls[0]![0]
    expect(sent).toMatchObject({ databaseIds: ['d1', 'd2', 'v1'], teams: ['team1'], people: ['rina@x'], level: 'read', brokerLevel: 'read', approval: 'label', lower: false, later: false })
  })

  it('leaves out what is unticked under On', async () => {
    api.planDBAccess.mockResolvedValue({ lines: [line({})] })
    api.giveDBAccess.mockResolvedValue({ lines: [line({})], rules: null })
    const dialog = await mount([db('d1', 'billing'), db('d2', 'orders')])
    fireEvent.click(within(dialog).getByRole('checkbox', { name: /orders/ }))
    fireEvent.click(await within(dialog).findByRole('button', { name: 'Give access on 1' }))
    await waitFor(() => expect(api.giveDBAccess).toHaveBeenCalled())
    expect(api.giveDBAccess.mock.calls[0]![0].databaseIds).toEqual(['d1'])
  })

  it('says when there is nothing to change', async () => {
    api.planDBAccess.mockResolvedValue({ lines: [line({ change: 'same', before: 'read', after: 'read', afterApproval: false })] })
    const dialog = await mount([db('d1', 'billing')])
    expect(await within(dialog).findByRole('button', { name: 'Nothing to change' })).toBeTruthy()
    expect(within(dialog).queryByText('Brokers · what they can do')).toBeNull()
  })
})

describe('the server list words', () => {
  it('counts kinds and names who has each one', () => {
    expect(countKinds([db('1', 'a'), db('2', 'b'), db('3', '/x', 'rabbitmq')])).toBe('2 databases, 1 broker')
    const rows = [{ teamId: 't', teamName: 'Payments', level: 'owner' }, { userEmail: 'rina@x', level: 'read' }] as DBAssignment[]
    expect(whoHasIt(rows, 'rabbitmq', (k) => k === 'u:rina@x' ? 'Rina' : k)).toBe('Payments · Manage, Rina · Read')
    expect(whoHasIt([], 'postgres', (k) => k)).toBe('Only admins')
  })
})
