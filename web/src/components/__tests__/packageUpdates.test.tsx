import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { UpdatesTab, heldBy, windowText } from '~/components/server/UpdatesTab'
import { PackageUpdateSettings } from '~/components/PackageUpdateSettings'
import { PackageFleetCard } from '~/components/PackageFleet'
import type { PackageFleetRow, PackagePolicy, PackageRun, PackagesView } from '~/lib/live'
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
  packageFleet: vi.fn(),
  startPackageRuns: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
}))

vi.mock('~/lib/packageApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/packageApi')>()),
  ...live,
}))

vi.mock('~/components/links', () => ({
  AnchorLink: ({ children }: { children: React.ReactNode }) => <a href="/assets">{children}</a>,
}))

const asset = { id: 'a1', hostname: 'web-01', protocol: 'ssh' } as Asset

const rule = {
  position: 0, tag: '', mode: 'notify' as const, windowDays: [0], windowStartMinute: 120, windowMinutes: 120,
  reboot: 'never' as const, needsApproval: false,
}

const view = (over: Partial<PackagesView> = {}): PackagesView => ({
  hostname: 'web-01', enrolled: true, reported: true, supported: true, canReboot: true, canUpgradeRelease: true,
  checking: false, checkAsked: false,
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

describe('Reboot', () => {
  const needs = (over: Partial<PackagesView> = {}) => {
    const v = view(over)
    return { ...v, report: { ...v.report!, reboot_required: true } }
  }

  it('says a reboot is needed where it shows, and reboots after asking', async () => {
    live.assetPackages.mockResolvedValue(needs())
    live.startPackageRun.mockResolvedValue(runOf({ mode: 'reboot', reboot: 'if_needed' }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText('This server needs a reboot')).toBeTruthy()
    expect(screen.getByText('Needed now')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Reboot now…' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('Reboot web-01?')).toBeTruthy()
    expect(within(dialog).getByText(/Nothing is installed/)).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Reboot web-01' }))
    await waitFor(() => expect(live.startPackageRun).toHaveBeenCalledWith('a1', { mode: 'reboot', reboot: 'if_needed' }))
  })

  it('does nothing when the reboot is cancelled', async () => {
    live.assetPackages.mockResolvedValue(needs())
    renderWith(<UpdatesTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Reboot now…' }))
    fireEvent.click(within(await screen.findByRole('dialog')).getByRole('button', { name: 'Cancel' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).toBeNull())
    expect(live.startPackageRun).not.toHaveBeenCalled()
  })

  it('says when the reboot waits for a second admin', async () => {
    live.assetPackages.mockResolvedValue(needs({ rule: { ...rule, needsApproval: true } }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Reboot now…' }))
    expect(within(await screen.findByRole('dialog')).getByText(/waits until another admin approves it/)).toBeTruthy()
  })

  it('shows no banner when no reboot is needed', async () => {
    live.assetPackages.mockResolvedValue(view())
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText('openssl')).toBeTruthy()
    expect(screen.queryByText('This server needs a reboot')).toBeNull()
    expect(screen.queryByRole('button', { name: 'Reboot now…' })).toBeNull()
  })

  it('tells someone who may not reboot who can, with no button', async () => {
    live.assetPackages.mockResolvedValue(needs())
    renderWith(<UpdatesTab asset={asset} canEdit={false} />)
    expect(await screen.findByText(/An admin or owner can reboot it from here/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Reboot now…' })).toBeNull()
  })

  it('asks to update an agent too old to reboot', async () => {
    live.assetPackages.mockResolvedValue(needs({ canReboot: false }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText(/too old to reboot from the console/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: 'Reboot now…' })).toBeNull()
  })

  it('waits for a run going before it offers a reboot', async () => {
    live.assetPackages.mockResolvedValue(needs({ runs: [runOf({ state: 'queued' })] }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText(/once the run going now has ended/)).toBeTruthy()
    expect((screen.getByRole('button', { name: 'Reboot now…' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('follows a reboot going in place of the banner', async () => {
    live.assetPackages.mockResolvedValue(needs({
      runs: [runOf({ mode: 'reboot', reboot: 'if_needed', state: 'rebooting', step: 'reboot',
        message: 'rebooting, as asked from the console', sentAt: new Date().toISOString() })],
    }))
    live.packageRun.mockResolvedValue(runOf({ mode: 'reboot', state: 'rebooting' }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText('rebooting, as asked from the console')).toBeTruthy()
    expect(screen.getAllByText('Rebooting').length).toBeGreaterThan(0)
    expect(screen.queryByText('This server needs a reboot')).toBeNull()
    expect(screen.queryByText('Refresh package lists')).toBeNull()
  })
})

describe('Release upgrade', () => {
  const jammy = { distro: 'ubuntu', current: 'Ubuntu 22.04.5 LTS', version: '22.04', next: '24.04',
    next_name: 'Ubuntu 24.04.1 LTS', tool: 'do-release-upgrade' }
  const ready = (release: object, over: Partial<PackagesView> = {}) => view({
    report: { checked_at: new Date().toISOString(), manager: 'apt', supported: true, utc_offset_minutes: 0, updates: [],
      release: { ...jammy, ...release } },
    ...over,
  })

  it('offers the next release and upgrades only after the server name is typed', async () => {
    live.assetPackages.mockResolvedValue(ready({}))
    live.startPackageRun.mockResolvedValue(runOf({ mode: 'release_upgrade', release: '24.04' }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText('New release available')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Upgrade to Ubuntu 24.04.1 LTS…' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/Holds do not apply/)).toBeTruthy()
    const go = within(dialog).getByRole('button', { name: 'Upgrade to Ubuntu 24.04.1 LTS' }) as HTMLButtonElement
    expect(go.disabled).toBe(true)
    fireEvent.change(within(dialog).getByLabelText('Type the server name to confirm'), { target: { value: 'web-01' } })
    expect(go.disabled).toBe(false)
    fireEvent.click(go)
    await waitFor(() => expect(live.startPackageRun).toHaveBeenCalledWith('a1',
      { mode: 'release_upgrade', release: '24.04', reboot: 'if_needed' }))
  })

  it('says what stands in the way, with no way to start', async () => {
    live.assetPackages.mockResolvedValue(ready({
      blockers: ['reboot it first: an update already installed waits for a reboot',
        'argus-control runs on this server, and upgrading it from the console would take the console down part way through; upgrade it by hand, as docs/os-upgrades.md describes'],
    }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText('Not ready yet')).toBeTruthy()
    expect(screen.getByText(/Reboot it first/)).toBeTruthy()
    expect(screen.getByText(/docs\/os-upgrades.md/)).toBeTruthy()
    expect((screen.getByRole('button', { name: 'Upgrade to Ubuntu 24.04.1 LTS…' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('installs what is waiting for the current release from the blocker itself', async () => {
    live.assetPackages.mockResolvedValue(view({
      report: { checked_at: new Date().toISOString(), manager: 'apt', supported: true, utc_offset_minutes: 0,
        updates: [{ name: 'libssl3t64', installed: '3.0.13-0ubuntu3.5', available: '3.0.13-0ubuntu3.6', origin: 'Ubuntu:24.04/noble-updates' }],
        release: { ...jammy, blockers: ['install the 1 update waiting for the current release first'] } },
    }))
    live.startPackageRun.mockResolvedValue(runOf({ mode: 'all' }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Install it now' }))
    const dialog = await screen.findByRole('dialog')
    fireEvent.click(within(dialog).getByRole('button', { name: /^Install/ }))
    await waitFor(() => expect(live.startPackageRun).toHaveBeenCalledWith('a1',
      expect.objectContaining({ mode: 'all' })))
  })

  it('asks to update an agent too old to upgrade, with no button', async () => {
    live.assetPackages.mockResolvedValue(ready({}, { canUpgradeRelease: false }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText(/too old to upgrade the operating system/)).toBeTruthy()
    expect(screen.queryByRole('button', { name: /Upgrade to/ })).toBeNull()
  })

  it('says why a supported server is not offered one, and nothing for one Argus does not upgrade', async () => {
    live.assetPackages.mockResolvedValue(ready({ next: undefined, next_name: undefined,
      reason: 'do-release-upgrade is not installed; install ubuntu-release-upgrader-core' }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText(/install ubuntu-release-upgrader-core/)).toBeTruthy()
  })

  it('follows a release upgrade going in place of the offer', async () => {
    live.assetPackages.mockResolvedValue(ready({}, {
      runs: [runOf({ mode: 'release_upgrade', release: '24.04', state: 'running', step: 'upgrade',
        message: 'upgrading to Ubuntu 24.04.1 LTS; this takes a while, and the server reboots at the end',
        sentAt: new Date().toISOString() })],
    }))
    live.packageRun.mockResolvedValue(runOf({ mode: 'release_upgrade', state: 'running', log: '' }))
    renderWith(<UpdatesTab asset={asset} canEdit />)
    expect(await screen.findByText('Release upgrade')).toBeTruthy()
    expect(screen.getByText(/upgrading to Ubuntu 24.04.1 LTS; this takes a while/)).toBeTruthy()
    expect(screen.queryByText('New release available')).toBeNull()
  })
})

describe('PackageFleetCard', () => {
  const row = (over: Partial<PackageFleetRow>): PackageFleetRow => ({
    assetId: 'a1', hostname: 'web-01', tags: [], os: 'Ubuntu 24.04', reported: true, supported: true, manager: 'apt',
    checkedAt: new Date().toISOString(), updates: 0, security: 0, rebootRequired: true, canReboot: true, mode: 'notify',
    run: null, ...over,
  })

  it('reboots the servers waiting for one, one at a time, installing nothing', async () => {
    live.packageFleet.mockResolvedValue([row({}), row({ assetId: 'a2', hostname: 'web-02' })])
    live.startPackageRuns.mockResolvedValue([])
    renderWith(<PackageFleetCard canEdit />)
    fireEvent.click(await screen.findByText('Reboot needed 2'))
    fireEvent.click(screen.getByLabelText('Update web-02'))
    fireEvent.click(screen.getByLabelText('Update web-01'))
    fireEvent.click(screen.getByRole('button', { name: 'Reboot 2 servers…' }))
    const dialog = await screen.findByRole('dialog')
    expect((within(dialog).getByLabelText('Reboot only; install nothing') as HTMLInputElement).checked).toBe(true)
    expect(within(dialog).queryByText('If an update needs a reboot')).toBeNull()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Reboot 2 servers' }))
    await waitFor(() => expect(live.startPackageRuns.mock.calls[0]?.[0]).toEqual(
      { assetIds: ['a2', 'a1'], mode: 'reboot', reboot: 'if_needed' }))
  })

  it('does not offer a reboot-only run when an agent is too old for one', async () => {
    live.packageFleet.mockResolvedValue([row({}), row({ assetId: 'a2', hostname: 'old-01', canReboot: false })])
    renderWith(<PackageFleetCard canEdit />)
    fireEvent.click(await screen.findByText('Reboot needed 2'))
    fireEvent.click(screen.getByLabelText('Update web-01'))
    fireEvent.click(screen.getByLabelText('Update old-01'))
    fireEvent.click(screen.getByRole('button', { name: 'Reboot 2 servers…' }))
    const dialog = await screen.findByRole('dialog')
    const only = within(dialog).getByLabelText('Reboot only; install nothing') as HTMLInputElement
    expect(only.disabled).toBe(true)
    expect(only.checked).toBe(false)
    expect(within(dialog).getByText(/Not for old-01/)).toBeTruthy()
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
