import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { UpdatesTab, heldBy, windowText } from '~/components/server/UpdatesTab'
import { PackageUpdateSettings } from '~/components/PackageUpdateSettings'
import type { PackagePolicy, PackageRun, PackagesView } from '~/lib/live'
import { theme } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * Operating system updates on a server's page and in Settings: what is
 * waiting, what is held back, what a press installs, and who may press.
 */

const live = vi.hoisted(() => ({
  assetPackages: vi.fn(),
  checkPackages: vi.fn(),
  startPackageRun: vi.fn(),
  decidePackageRun: vi.fn(),
  packageRun: vi.fn(),
  packagePolicy: vi.fn(),
  savePackagePolicy: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
}))

vi.mock('~/lib/packageApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/packageApi')>()),
  ...live,
}))

const asset = { id: 'a1', hostname: 'web-01', protocol: 'ssh' } as Asset

const rule = {
  position: 0, tag: '', mode: 'notify' as const, windowDays: [0], windowStartMinute: 120, windowMinutes: 120,
  reboot: 'never' as const, needsApproval: false,
}

const view = (over: Partial<PackagesView> = {}): PackagesView => ({
  hostname: 'web-01', enrolled: true, reported: true, supported: true, checking: false, checkAsked: false,
  report: {
    checked_at: new Date().toISOString(), manager: 'apt', supported: true, utc_offset_minutes: 0,
    updates: [
      { name: 'openssl', installed: '3.0.13-0ubuntu3.4', available: '3.0.13-0ubuntu3.5', security: true, origin: 'noble-security' },
      { name: 'vim', installed: '2:9.1.0016-1ubuntu7.5', available: '2:9.1.0016-1ubuntu7.6', origin: 'noble-updates' },
      { name: 'postgresql-16', installed: '16.4', available: '16.6', security: true },
      { name: 'linux-image-6.8.0-50-generic', available: '6.8.0-50.51', new: true, reboot: true },
    ],
  },
  rule, holds: ['postgresql*'], nextWindow: null, runs: [], ...over,
})

const runOf = (over: Partial<PackageRun>): PackageRun => ({
  id: 'r1', hostname: 'web-01', assetHostname: 'web-01', mode: 'all', packages: [], holds: [], reboot: 'never',
  trigger: 'manual', state: 'queued', requestedBy: 'ada@example.com', requestedAt: new Date().toISOString(),
  decidedBy: null, decidedAt: null, sentAt: null, startedAt: null, finishedAt: null, updatedAt: new Date().toISOString(),
  step: '', message: '', planned: 0, upgraded: [], restarted: [], rebootRequired: false, rebooted: false,
  batchId: null, batchPosition: null, ...over,
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
  it('matches holds as the agent does, by full name and by name before the architecture', () => {
    expect(heldBy('postgresql-16', ['postgresql*'])).toBe('postgresql*')
    expect(heldBy('curl.x86_64', ['curl'])).toBe('curl')
    expect(heldBy('openssl', ['postgresql*'])).toBeUndefined()
    expect(heldBy('a+b', ['a+b'])).toBe('a+b')
  })
  it('says a window in words', () => {
    expect(windowText({ windowDays: [0, 3], windowStartMinute: 120, windowMinutes: 120 })).toBe('Sun, Wed 02:00–04:00')
    expect(windowText({ windowDays: [0, 1, 2, 3, 4, 5, 6], windowStartMinute: 1380, windowMinutes: 180 })).toBe('Daily 23:00–02:00')
  })
})

describe('UpdatesTab', () => {
  it('shows what is waiting, with held and pulled-in packages left unticked', async () => {
    live.assetPackages.mockResolvedValue(view())
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText('openssl')).toBeTruthy()
    expect(screen.getByText('Held · postgresql*')).toBeTruthy()
    expect((screen.getByLabelText('Install openssl') as HTMLInputElement).checked).toBe(true)
    expect((screen.getByLabelText('Install postgresql-16') as HTMLInputElement).checked).toBe(false)
    expect((screen.getByLabelText('Install linux-image-6.8.0-50-generic') as HTMLInputElement).checked).toBe(false)
    expect(screen.getByRole('button', { name: 'Update 2 packages' })).toBeTruthy()
    expect(screen.getByText('Will be needed')).toBeTruthy()
  })

  it('installs security updates only, rebooting if asked', async () => {
    live.assetPackages.mockResolvedValue(view())
    live.startPackageRun.mockResolvedValue(runOf({}))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Security only' }))
    const dialog = await screen.findByRole('dialog')
    // postgresql-16 is a security update, but held: one goes.
    expect(within(dialog).getByText(/Install 1 security update on web-01/)).toBeTruthy()
    fireEvent.click(within(dialog).getByLabelText('Reboot at the end of the run'))
    fireEvent.click(within(dialog).getByRole('button', { name: 'Install 1 security update' }))
    await waitFor(() => expect(live.startPackageRun).toHaveBeenCalledWith('a1', { mode: 'security', reboot: 'if_needed' }))
  })

  it('installs everything waiting as "all", and a hand-picked held package by name', async () => {
    live.assetPackages.mockResolvedValue(view())
    live.startPackageRun.mockResolvedValue(runOf({}))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Update 2 packages' }))
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: /Install all 2/ }))
    await waitFor(() => expect(live.startPackageRun).toHaveBeenCalledWith('a1', { mode: 'all', reboot: 'never' }))

    live.startPackageRun.mockClear()
    fireEvent.click(screen.getByLabelText('Install vim'))
    fireEvent.click(screen.getByLabelText('Install postgresql-16'))
    fireEvent.click(screen.getByRole('button', { name: 'Update 2 packages' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/Picked by hand although held: postgresql-16/)).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Install 2 packages' }))
    await waitFor(() => expect(live.startPackageRun).toHaveBeenCalledWith('a1',
      { mode: 'selected', reboot: 'never', packages: ['openssl', 'postgresql-16'] }))
  })

  it('offers nothing to install to someone who may not', async () => {
    live.assetPackages.mockResolvedValue(view())
    renderWith(<UpdatesTab asset={asset} canEdit={false} />)
    expect(await screen.findByText('openssl')).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Update/ })).toBeNull()
    expect(screen.queryByRole('button', { name: 'Check now' })).toBeNull()
    expect(screen.queryByLabelText('Install openssl')).toBeNull()
  })

  it('says why updates are not available', async () => {
    live.assetPackages.mockResolvedValue(view({ reported: false, report: null }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText(/too old to report operating system updates/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Update/ })).toBeNull()
  })

  it('repeats what the agent says when it cannot update the server', async () => {
    live.assetPackages.mockResolvedValue(view({ supported: false, reason: 'the agent does not run as root', report: null }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText(/not available on this server: the agent does not run as root/)).toBeTruthy()
  })

  it('is for Linux servers', async () => {
    renderWith(<UpdatesTab asset={{ ...asset, protocol: 'rdp' } as Asset} canEdit />)
    expect(await screen.findByText(/for Linux servers/)).toBeTruthy()
    expect(live.assetPackages).not.toHaveBeenCalled()
  })

  it('shows a run going, and does not start another meanwhile', async () => {
    live.assetPackages.mockResolvedValue(view({
      runs: [runOf({ state: 'running', step: 'install', planned: 2, message: 'installing 2 packages', sentAt: new Date().toISOString() })],
    }))
    live.packageRun.mockResolvedValue(runOf({ state: 'running', log: 'Unpacking openssl (3.0.13-0ubuntu3.5) ...' }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText('Install 2 packages')).toBeTruthy()
    expect(await screen.findByText(/Unpacking openssl/)).toBeTruthy()
    expect((screen.getByRole('button', { name: 'Update 2 packages' }) as HTMLButtonElement).disabled).toBe(true)
    // A run the server has started cannot be taken back.
    expect(screen.queryByRole('button', { name: 'Take back' })).toBeNull()
  })

  it('lets another admin approve a run, but not the one who asked', async () => {
    live.assetPackages.mockResolvedValue(view({ runs: [runOf({ state: 'awaiting_approval' })] }))
    live.decidePackageRun.mockResolvedValue(runOf({ state: 'queued' }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Approve' }))
    await waitFor(() => expect(live.decidePackageRun).toHaveBeenCalledWith('r1', 'approve'))
  })

  it('does not offer approval to the one who asked', async () => {
    live.assetPackages.mockResolvedValue(view({ runs: [runOf({ state: 'awaiting_approval' })] }))
    renderWith(<UpdatesTab asset={asset} canEdit />, 'ada@example.com')
    expect(await screen.findByText('Waiting for approval')).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Approve' })).toBeNull()
    expect(screen.getByRole('button', { name: 'Take back' })).toBeTruthy()
  })
})

describe('PackageUpdateSettings', () => {
  const policy = (): PackagePolicy => ({
    rules: [
      { id: 'p', position: 0, tag: 'production', mode: 'security', windowDays: [0], windowStartMinute: 120, windowMinutes: 120, reboot: 'if_needed', needsApproval: true },
      { id: 'd', position: 1, tag: '', mode: 'notify', windowDays: [0], windowStartMinute: 120, windowMinutes: 120, reboot: 'never', needsApproval: false },
    ],
    holds: ['postgresql*'],
  })

  it('saves a changed rule with the rule for every other server last', async () => {
    live.packagePolicy.mockResolvedValue(policy())
    live.savePackagePolicy.mockImplementation(async (p: PackagePolicy) => p)
    renderWith(<PackageUpdateSettings canEdit />)
    const tags = await screen.findByLabelText('Servers tagged')
    fireEvent.change(tags, { target: { value: 'prod' } })
    fireEvent.click(await screen.findByRole('button', { name: 'Save' }))
    await waitFor(() => expect(live.savePackagePolicy).toHaveBeenCalled())
    const sent = live.savePackagePolicy.mock.calls[0]![0] as PackagePolicy
    expect(sent.rules.map((r) => r.tag)).toEqual(['prod', ''])
    expect(sent.rules.map((r) => r.position)).toEqual([0, 1])
    expect(sent.holds).toEqual(['postgresql*'])
  })

  it('is read-only for an auditor', async () => {
    live.packagePolicy.mockResolvedValue(policy())
    renderWith(<PackageUpdateSettings canEdit={false} />)
    expect(((await screen.findByLabelText('Servers tagged')) as HTMLInputElement).disabled).toBe(true)
    expect(screen.queryByRole('button', { name: 'Add rule' })).toBeNull()
  })
})
