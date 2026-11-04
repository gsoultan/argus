import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { ReactNode } from 'react'
import { BackupSettings, BackupsPanel, describeTests, dur } from '~/components/DatabaseBackups'
import { renderWithProviders } from '~/test/render'
import type { BackupSchedule, BackupTest, BackupsView, Database, DatabaseBackup } from '~/types/domain'

/**
 * Test restores in the console: the backups list says which backup is proven
 * to restore and how long it took, a report opens from it, and Settings says
 * when tests run and lets an admin try their checks before saving them.
 */

const live = vi.hoisted(() => ({
  databaseBackups: vi.fn(),
  backupCatalog: vi.fn(),
  backupTest: vi.fn(),
  testBackup: vi.fn(),
  tryBackupChecks: vi.fn(),
  setBackupSchedule: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  ...live,
}))

vi.mock('@tanstack/react-router', async (orig) => ({
  ...(await orig<typeof import('@tanstack/react-router')>()),
  Link: ({ children }: { children: ReactNode }) => <a>{children}</a>,
}))

const db: Database = {
  id: 'db1', assetId: 'a1', hostname: 'db-01', engine: 'postgres', host: '127.0.0.1', port: 5432, poolerHost: '', poolerPort: 0,
  dbname: 'shop', adminUser: 'postgres', label: 'production' as Database['label'], policy: {} as Database['policy'],
  createdBy: 'ann@example.com', createdAt: '2026-10-01T00:00:00Z', tunnelConnected: true,
}

const schedule = (over: Partial<BackupSchedule> = {}): BackupSchedule => ({
  databaseId: 'db1', enabled: true, every: 'daily', atHour: 2, atMinute: 0, weekday: 0, destination: 'server', keepLast: 7, keepDays: 0,
  nextRunAt: null, misses: 0, updatedBy: 'ann@example.com', updatedAt: '2026-11-01T00:00:00Z', exists: true,
  testEvery: 'weekly', testHour: 4, testMinute: 0, testWeekday: 0, testChecks: [], ...over,
})

const backup = (id: string, startedAt: string): DatabaseBackup => ({
  id, databaseId: 'db1', startedBy: 'schedule:db1', reason: '', state: 'done', stage: '', detail: '', error: '', destination: 'server',
  onServer: true, onControl: false, bytes: 1 << 30, sha256: '', keyId: 'k', serverVersion: '16.4', dumpVersion: '16.4', verified: true,
  pinned: false, startedAt, finishedAt: startedAt, reportedAt: startedAt,
})

const test = (over: Partial<BackupTest>): BackupTest => ({
  id: 't1', backupId: 'b1', databaseId: 'db1', startedBy: 'schedule:db1', state: 'passed', stage: '', detail: '', error: '',
  scratchName: 'argus_test_0a1b2c3d', scratchDropped: true, restoreMs: 252_000, tables: 41, totalRows: 1_204_311, bytes: 2 << 30,
  report: {
    steps: [{ name: 'Restore', ok: true, ms: 252_000 }],
    checks: [
      { name: 'Every table is there', kind: 'builtin', state: 'ok', value: '41 of 41' },
      { name: 'Paid orders', kind: 'custom', state: 'ok', value: 'true' },
    ],
    tables: null, tablesTotal: 41, indexes: 120, invalidIndexes: 0, warnings: 0, needBytes: 0, freeBytes: 0, sourceBytes: 0,
  },
  startedAt: '2026-11-02T04:00:00Z', finishedAt: '2026-11-02T04:05:00Z', reportedAt: '2026-11-02T04:05:00Z',
  backupStartedAt: '2026-11-02T02:00:00Z', ...over,
})

const view = (over: Partial<BackupsView> = {}): BackupsView => ({
  backups: [backup('b2', '2026-11-03T02:00:00Z'), backup('b1', '2026-11-02T02:00:00Z')],
  running: null, schedule: schedule(), restores: [], level: 'dba', storage: false, staleAfterS: 120,
  tests: [test({ id: 't2', backupId: 'b2', state: 'failed', error: 'orders is missing', restoreMs: 0 }), test({})],
  testing: null, lastGood: test({}), ...over,
})

afterEach(() => vi.clearAllMocks())

describe('test restores', () => {
  it('words the schedule and durations', () => {
    expect(describeTests(schedule())).toBe('test restore every Sunday at 04:00')
    expect(describeTests(schedule({ testEvery: 'after' }))).toBe('test restore after every backup')
    expect(describeTests(schedule({ testEvery: 'off' }))).toBe('no test restores')
    expect(dur(850)).toBe('850 ms')
    expect(dur(252_000)).toBe('4 min 12 s')
    expect(dur(3_780_000)).toBe('1 h 3 min')
  })

  it('shows the last known good, the Tested column and a failure since', async () => {
    live.databaseBackups.mockResolvedValue(view())
    live.backupTest.mockResolvedValue({ ...test({}), report: { ...test({}).report, tables: [{ name: 'public.orders', rows: 900_000, previous: 880_000 }] } })
    renderWithProviders(<BackupsPanel database={db} level="dba" narrow={false} onBack={() => {}} admin />)
    expect(await screen.findByText('Last known good:')).toBeTruthy()
    expect(screen.getByText('test failed')).toBeTruthy()
    expect(screen.getByText('last known good')).toBeTruthy()
    expect(screen.getByText(/test restore every Sunday at 04:00/)).toBeTruthy()
    expect(screen.getByText(/failed .*orders is missing/)).toBeTruthy()

    fireEvent.click(screen.getByText('restored in 4 min 12 s'))
    expect(await screen.findByText('public.orders')).toBeTruthy()
    expect(live.backupTest).toHaveBeenCalledWith('db1', 't1')
    expect(screen.getByText('Paid orders')).toBeTruthy()
  })

  it('nudges when backups are scheduled and never tested', async () => {
    live.databaseBackups.mockResolvedValue(view({ schedule: schedule({ testEvery: 'off' }), tests: [], lastGood: null }))
    renderWithProviders(<BackupsPanel database={db} level="dba" narrow={false} onBack={() => {}} admin />)
    expect(await screen.findByText('never test-restored on a schedule')).toBeTruthy()
    expect(screen.queryByText('Last known good:')).toBeNull()
  })

  it('tries checks on the live database and saves them with the schedule', async () => {
    live.databaseBackups.mockResolvedValue(view())
    live.backupCatalog.mockRejectedValue(new Error('offline'))
    live.tryBackupChecks.mockResolvedValue({ checks: [{ name: 'Paid orders', ok: true, value: 'true' }] })
    live.setBackupSchedule.mockImplementation(async (_id: string, s: Partial<BackupSchedule>) => ({ ...schedule(), ...s }))
    renderWithProviders(<BackupSettings db={db} />)
    fireEvent.click(await screen.findByText('Add a check'))
    fireEvent.change(screen.getByLabelText('Check name'), { target: { value: 'Paid orders' } })
    fireEvent.change(screen.getByLabelText('Check SQL'), { target: { value: "SELECT count(*) > 0 FROM orders WHERE state = 'paid'" } })
    fireEvent.click(screen.getByText('Try them on the live database'))
    expect(await screen.findByText('true on the live database now')).toBeTruthy()
    expect(live.tryBackupChecks).toHaveBeenCalledWith('db1', [{ name: 'Paid orders', sql: "SELECT count(*) > 0 FROM orders WHERE state = 'paid'" }])

    fireEvent.click(screen.getByText('Save'))
    await waitFor(() => expect(live.setBackupSchedule).toHaveBeenCalled())
    expect(live.setBackupSchedule.mock.calls[0]?.[1]).toMatchObject({
      testEvery: 'weekly', testWeekday: 0, testHour: 4,
      testChecks: [{ name: 'Paid orders', sql: "SELECT count(*) > 0 FROM orders WHERE state = 'paid'" }],
    })
  })
})
