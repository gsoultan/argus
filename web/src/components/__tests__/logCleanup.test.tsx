import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { cleanable, diskFilling, diskPercent, LogsTab, manifestCsv, mountOf } from '~/components/server/LogsTab'
import { LogCleanupSettings } from '~/components/LogCleanupSettings'
import type { LogCleanupPolicy, LogRun, LogsView } from '~/lib/live'
import { theme } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * Log cleanup on a server's page and in Settings: what can be freed, what a
 * press removes, the typed confirm, and who may press.
 */

const live = vi.hoisted(() => ({
  assetLogs: vi.fn(),
  scanLogs: vi.fn(),
  startLogRun: vi.fn(),
  decideLogRun: vi.fn(),
  logRun: vi.fn(),
  logPolicy: vi.fn(),
  saveLogPolicy: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
}))

vi.mock('~/lib/logApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/logApi')>()),
  ...live,
}))

// The tab links to Settings; no router here.
vi.mock('~/components/links', () => ({
  ButtonLink: ({ children }: { children: React.ReactNode }) => <a href="/settings">{children}</a>,
}))

const asset = { id: 'a1', hostname: 'web-01.impack.local', protocol: 'ssh' } as Asset
const GB = 1024 ** 3
const MB = 1024 ** 2

const rule = {
  position: 0, tag: '', mode: 'off' as const, diskPercent: 85, windowStartMinute: 180, windowMinutes: 120,
  rotatedKeepDays: 14, journalKeepDays: 28, journalMaxMb: 2048, activeOverMb: 0, activeKeepMb: 100, needsApproval: false,
}

const view = (over: Partial<LogsView> = {}): LogsView => ({
  hostname: 'web-01.impack.local', enrolled: true, reported: true, supported: true, reason: '', scanning: false,
  scanAsked: false, confirm: 'web-01', rule, runs: [],
  report: {
    scanned_at: new Date().toISOString(), os: 'linux', supported: true, active_over: 0, utc_offset_minutes: 0,
    disks: [{ mount: '/', fs: 'ext4', total: 80 * GB, used: 74 * GB, avail: 6 * GB }],
    items: [
      { key: 'journal', kind: 'journal', title: 'Journal', detail: 'systemd journal', root: '/var/log/journal', bytes: 4 * GB, count: 1 },
      { key: 'rotated', kind: 'rotated', title: 'Rotated logs', root: '/var/log', bytes: 6 * GB, count: 412 },
      { key: 'active:/var/log/nginx/access.log', kind: 'active', title: 'nginx/access.log', root: '/var/log', bytes: 4 * GB, count: 1 },
    ],
    open: [{ pid: 2381, fd: 4, process: 'java', unit: 'tomcat.service', path: '/opt/tomcat/logs/catalina.out', bytes: 2 * GB }],
    protected: [{ title: 'Login records', detail: 'auth.log, wtmp, btmp, lastlog', bytes: 212 * MB }],
  },
  previews: [
    { key: 'journal', does: 'Keep 4 weeks, at most 2 GB', frees: 2 * GB, files: 0, automatic: false },
    { key: 'rotated', does: 'Delete older than 14 days', frees: 6 * GB, files: 412, automatic: false },
    { key: 'active:/var/log/nginx/access.log', does: 'Empty, keep the last 100 MB', frees: 4 * GB - 100 * MB, files: 0, automatic: false },
  ],
  ...over,
})

const runOf = (over: Partial<LogRun>): LogRun => ({
  id: 'r1', hostname: 'web-01', assetHostname: 'web-01.impack.local', trigger: 'manual', state: 'queued', items: ['rotated'],
  summary: 'Rotated logs (412 files)', plannedBytes: 6 * GB, reason: '', requestedBy: 'ada@example.com',
  requestedAt: new Date().toISOString(), decidedBy: null, decidedAt: null, sentAt: null, startedAt: null, finishedAt: null,
  updatedAt: new Date().toISOString(), step: '', message: '', freed: 0, removedCount: 0, removedMore: 0, skipped: [], ...over,
})

function renderWith(node: React.ReactNode, email = 'grace@example.com') {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  qc.setQueryData(['me'], { email, role: 'admin' })
  render(
    <QueryClientProvider client={qc}>
      <MantineProvider theme={theme}>{node}</MantineProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.clearAllMocks())

describe('helpers', () => {
  it('counts a disk as df does and finds the disk a path is on', () => {
    expect(diskPercent(74, 6)).toBe(93)
    const disks = [{ mount: '/', fs: '', total: 0, used: 0, avail: 0 }, { mount: '/var', fs: '', total: 0, used: 0, avail: 0 },
      { mount: 'D:\\', fs: '', total: 0, used: 0, avail: 0 }]
    expect(mountOf('/var/log/syslog', disks)).toBe('/var')
    expect(mountOf('/variable', disks)).toBe('/')
    expect(mountOf('d:\\apps\\logs', disks)).toBe('D:\\')
  })
  it('adds up what can be freed and says when a disk is filling', () => {
    expect(cleanable(view())).toBe(2 * GB + 6 * GB + 4 * GB - 100 * MB + 2 * GB)
    expect(diskFilling(view())).toBe(true)
  })
  it('writes the manifest as CSV, quoting what needs it', () => {
    expect(manifestCsv([{ path: '/var/log/a,b.log', action: 'deleted', bytes: 10 }])).toBe(
      'path,action,bytes,bytes_after,modified\n"/var/log/a,b.log",deleted,10,,\n')
  })
})

describe('LogsTab', () => {
  it('shows the logs with old ones picked and live ones not', async () => {
    live.assetLogs.mockResolvedValue(view())
    renderWith(<LogsTab asset={asset} canEdit />)
    expect(await screen.findByText('Rotated logs')).toBeTruthy()
    expect((screen.getByLabelText('Clean Journal') as HTMLInputElement).checked).toBe(true)
    expect((screen.getByLabelText('Clean Rotated logs') as HTMLInputElement).checked).toBe(true)
    expect((screen.getByLabelText('Clean nginx/access.log') as HTMLInputElement).checked).toBe(false)
    expect(screen.getByRole('button', { name: 'Clean 8 GB…' })).toBeTruthy()
    expect(screen.getByText('93% used')).toBeTruthy()
    expect(screen.getByText('Login records')).toBeTruthy()
  })

  it('asks for the server name typed before it cleans', async () => {
    live.assetLogs.mockResolvedValue(view())
    live.startLogRun.mockResolvedValue(runOf({}))
    renderWith(<LogsTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByLabelText('Clean Journal'))
    fireEvent.click(await screen.findByRole('button', { name: 'Clean 6 GB…' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/Clean 6 GB on web-01/)).toBeTruthy()
    const go = within(dialog).getByRole('button', { name: 'Clean 6 GB' }) as HTMLButtonElement
    expect(go.disabled).toBe(true)
    fireEvent.change(within(dialog).getByLabelText(/to confirm/), { target: { value: 'Web-01 ' } })
    expect(go.disabled).toBe(false)
    fireEvent.click(go)
    await waitFor(() => expect(live.startLogRun).toHaveBeenCalledWith('a1',
      { items: ['rotated'], open: [], restart: [], eventLogs: [], confirm: 'Web-01' }))
  })

  it('offers to restart the service holding a deleted file, or to empty it', async () => {
    live.assetLogs.mockResolvedValue(view({ rule: { ...rule, needsApproval: true } }))
    live.startLogRun.mockResolvedValue(runOf({ state: 'awaiting_approval' }))
    renderWith(<LogsTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Restart tomcat.service' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/second admin/)).toBeTruthy()
    fireEvent.change(within(dialog).getByLabelText(/to confirm/), { target: { value: 'web-01' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Ask for approval' }))
    await waitFor(() => expect(live.startLogRun).toHaveBeenCalledWith('a1',
      { items: [], open: [], restart: ['tomcat.service'], eventLogs: [], confirm: 'web-01' }))
  })

  it('only raises a Windows event log maximum, never clears one', async () => {
    const v = view()
    v.report!.os = 'windows'
    v.report!.open = []
    v.report!.event_logs = [{ name: 'Security', bytes: 20 * MB, max_bytes: 20 * MB, protected: true }]
    live.assetLogs.mockResolvedValue(v)
    live.startLogRun.mockResolvedValue(runOf({}))
    renderWith(<LogsTab asset={{ ...asset, protocol: 'rdp' } as Asset} canEdit />)
    expect(await screen.findByText('Overwriting')).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Clear/ })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Raise maximum' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/Nothing is removed/)).toBeTruthy()
    fireEvent.change(within(dialog).getByLabelText(/to confirm/), { target: { value: 'web-01' } })
    fireEvent.click(within(dialog).getByRole('button', { name: 'Raise maximum' }))
    await waitFor(() => expect(live.startLogRun).toHaveBeenCalledWith('a1',
      { items: [], open: [], restart: [], eventLogs: [{ name: 'Security', maxMb: 64 }], confirm: 'web-01' }))
  })

  it('offers nothing to clean to someone who may not', async () => {
    live.assetLogs.mockResolvedValue(view())
    renderWith(<LogsTab asset={asset} canEdit={false} />)
    expect(await screen.findByText('Rotated logs')).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Clean/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Scan now' })).toBeNull()
    expect(screen.queryByRole('button', { name: /Restart/ })).toBeNull()
    expect(screen.queryByLabelText('Clean Journal')).toBeNull()
  })

  it('says why logs cannot be cleaned', async () => {
    live.assetLogs.mockResolvedValue(view({ reported: false, report: null }))
    renderWith(<LogsTab asset={asset} canEdit />)
    expect(await screen.findByText(/too old to report its logs/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Clean/ })).toBeNull()
  })

  it('repeats what the agent says when it cannot clean the server', async () => {
    live.assetLogs.mockResolvedValue(view({ supported: false, reason: 'the agent does not run as root', report: null }))
    renderWith(<LogsTab asset={asset} canEdit />)
    expect(await screen.findByText(/cannot be cleaned on this server from the console: the agent does not run as root/)).toBeTruthy()
  })

  it('lets another admin approve a cleanup, but not the one who asked', async () => {
    live.assetLogs.mockResolvedValue(view({ runs: [runOf({ state: 'awaiting_approval' })] }))
    live.decideLogRun.mockResolvedValue(runOf({ state: 'queued' }))
    renderWith(<LogsTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(live.decideLogRun).toHaveBeenCalledWith('r1', 'approve'))
    expect((screen.getByRole('button', { name: 'Clean 8 GB…' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('does not offer approval to the one who asked', async () => {
    live.assetLogs.mockResolvedValue(view({ runs: [runOf({ state: 'awaiting_approval' })] }))
    renderWith(<LogsTab asset={asset} canEdit />, 'ada@example.com')
    expect(await screen.findByText('Waiting for approval')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Take back' })).toBeTruthy()
  })

  it('shows what a past cleanup removed', async () => {
    const done = runOf({ state: 'done', freed: 6 * GB, removedCount: 2, finishedAt: new Date().toISOString(),
      skipped: [{ path: '/var/log/syslog.1', why: 'it grew since the scan' }] })
    live.assetLogs.mockResolvedValue(view({ runs: [done] }))
    live.logRun.mockResolvedValue({ ...done, removed: [
      { path: '/var/log/nginx/access.log.4.gz', action: 'deleted', bytes: 212 * MB, modified: '2026-10-19T00:00:00Z' },
      { path: '/var/log/nginx/access.log', action: 'truncated', bytes: 4 * GB, after: 100 * MB },
    ] })
    renderWith(<LogsTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByText(/freed 6 GB · 2 files/))
    expect(await screen.findByText('/var/log/nginx/access.log.4.gz')).toBeTruthy()
    expect(screen.getByText('4 GB → 100 MB')).toBeTruthy()
    expect(screen.getByText('1 file skipped')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Download CSV' })).toBeTruthy()
  })
})

describe('LogCleanupSettings', () => {
  const policy = (): LogCleanupPolicy => ({
    rules: [
      { ...rule, id: 'p', position: 0, tag: 'production', mode: 'disk', needsApproval: true },
      { ...rule, id: 'd', position: 1 },
    ],
    paths: ['/srv/*/logs/*.log'],
    protect: [],
  })

  it('is off by default and saves a changed rule with the default last', async () => {
    live.logPolicy.mockResolvedValue(policy())
    live.saveLogPolicy.mockImplementation(async (p: LogCleanupPolicy) => p)
    renderWith(<LogCleanupSettings canEdit />)
    const offs = await screen.findAllByLabelText('Off')
    expect((offs[0] as HTMLInputElement).checked).toBe(true)
    fireEvent.change(screen.getByLabelText('Servers tagged'), { target: { value: 'prod' } })
    fireEvent.click(await screen.findByRole('button', { name: 'Save' }))
    await waitFor(() => expect(live.saveLogPolicy).toHaveBeenCalled())
    const sent = live.saveLogPolicy.mock.calls[0]![0] as LogCleanupPolicy
    expect(sent.rules.map((r) => r.tag)).toEqual(['prod', ''])
    expect(sent.rules.map((r) => r.position)).toEqual([0, 1])
    expect(sent.paths).toEqual(['/srv/*/logs/*.log'])
    expect(screen.getByText('Windows Security log')).toBeTruthy()
  })

  it('is read-only for an auditor', async () => {
    live.logPolicy.mockResolvedValue(policy())
    renderWith(<LogCleanupSettings canEdit={false} />)
    expect(((await screen.findByLabelText('Servers tagged')) as HTMLInputElement).disabled).toBe(true)
    expect(screen.queryByRole('button', { name: 'Add rule' })).toBeNull()
  })
})
