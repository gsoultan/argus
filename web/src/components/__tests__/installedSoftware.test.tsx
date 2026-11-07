import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { InstalledSoftware } from '~/components/server/InstalledSoftware'
import type { PackageRun } from '~/lib/live'
import { sizeText, softwareRunText, type SoftwareAnswer, type SoftwarePreview } from '~/lib/softwareApi'
import { theme } from '~/theme'
import type { Asset, ServerApp } from '~/types/domain'

/**
 * The Installed segment of What runs here: what is installed, what cannot
 * be removed, searching and installing with a preview, removing with the
 * server's name typed, and undoing a finished change.
 */

const api = vi.hoisted(() => ({
  installedSoftware: vi.fn(),
  searchSoftware: vi.fn(),
  previewSoftware: vi.fn(),
  startSoftwareRun: vi.fn(),
  softwareRuns: vi.fn(),
  undoSoftwareRun: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
}))

vi.mock('~/lib/softwareApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/softwareApi')>()),
  ...api,
}))

const asset = { id: 'a1', hostname: 'web-01.example', protocol: 'ssh' } as Asset

const installed: SoftwareAnswer = {
  state: 'ok', manager: 'apt', total: 1481, manual: 3, found: [],
  installed: [
    { name: 'nginx', version: '1.24.0-2ubuntu7.2', origin: 'noble-updates/main', size_kb: 1400, manual: true },
    { name: 'redis-server', version: '5:7.0.15-1build2', manual: true, keeps_data: true },
    { name: 'openssh-server', version: '1:9.6p1-3ubuntu13.7', manual: true, protected: 'how Argus reaches it' },
  ],
}

const nginxApp = {
  kind: 'nginx', name: 'nginx', version: '1.24.0', running: true, process: 'nginx', unit: 'nginx.service',
  package: 'nginx', container: '', image: '', evidence: ['package'],
  listen: [{ address: '0.0.0.0', port: 443 }, { address: '0.0.0.0', port: 80 }],
} as unknown as ServerApp

const previewOf = (over: Partial<SoftwarePreview>): SoftwarePreview => ({
  id: 'p1', assetId: 'a1', action: 'install', items: [{ name: 'redis-tools' }], purge: false, startServices: true,
  result: {
    action: 'install', purge_allowed: false, hash: 'a'.repeat(64), download_kb: 1600, disk_kb: 6200,
    changes: [
      { kind: 'add', name: 'redis-tools', to: '5:7.0.15-1build2', asked: true },
      { kind: 'upgrade', name: 'libjemalloc2', from: '5.3.0-2', to: '5.3.0-2build1' },
    ],
    services: [],
  },
  needsApproval: false, createdBy: 'grace@example.com', createdAt: new Date().toISOString(),
  expiresAt: new Date(Date.now() + 15 * 60_000).toISOString(), ...over,
})

const runOf = (over: Partial<PackageRun>): PackageRun => ({
  id: 'r1', hostname: 'web-01', assetHostname: 'web-01.example', mode: 'install', packages: [], holds: [], reboot: 'never',
  trigger: 'manual', state: 'done', requestedBy: 'ada@example.com', requestedAt: new Date().toISOString(),
  decidedBy: null, decidedAt: null, sentAt: null, startedAt: null, finishedAt: new Date().toISOString(),
  updatedAt: new Date().toISOString(), step: '', message: '', planned: 0, upgraded: [], restarted: [],
  rebootRequired: false, rebooted: false, batchId: null, batchPosition: null, items: [], started: [], ...over,
})

function renderWith(node: React.ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } })
  qc.setQueryData(['me'], { email: 'grace@example.com', role: 'admin' })
  render(
    <QueryClientProvider client={qc}>
      <MantineProvider theme={theme}>{node}</MantineProvider>
    </QueryClientProvider>,
  )
}

afterEach(() => vi.clearAllMocks())

describe('helpers', () => {
  it('says sizes and runs in words', () => {
    expect(sizeText(1400)).toBe('1.4 MB')
    expect(sizeText(512)).toBe('512 KB')
    expect(sizeText(undefined)).toBe('—')
    expect(softwareRunText(runOf({ items: [{ name: 'redis-server' }],
      upgraded: [{ name: 'redis-server', to: '7' }, { name: 'redis-tools', to: '7' }, { name: 'liblzf1', to: '3' }] })))
      .toBe('installed redis-server and 2 others')
    expect(softwareRunText(runOf({ mode: 'remove', purge: true, state: 'awaiting_approval', items: [{ name: 'nginx' }] })))
      .toBe('remove nginx and its settings')
  })
})

describe('InstalledSoftware', () => {
  it('lists what is installed, what runs from it, and never offers to remove what is protected', async () => {
    api.installedSoftware.mockResolvedValue(installed)
    api.softwareRuns.mockResolvedValue({ runs: [], needsApproval: false })
    renderWith(<InstalledSoftware asset={asset} canEdit apps={[nginxApp]} active />)
    expect(await screen.findByText('nginx')).toBeTruthy()
    expect(screen.getByText(':443 :80')).toBeTruthy()
    expect(screen.getByText('Protected')).toBeTruthy()
    expect(screen.getByLabelText('Actions for nginx')).toBeTruthy()
    expect(screen.queryByLabelText('Actions for openssh-server')).toBeNull()
    expect(screen.getByText(/apt · 3 asked for, 1481 with libraries/)).toBeTruthy()

    fireEvent.click(screen.getByLabelText('Show libraries and dependencies'))
    await waitFor(() => expect(api.installedSoftware).toHaveBeenCalledWith('a1', true))
  })

  it('does not ask the agent while the segment is closed', async () => {
    renderWith(<InstalledSoftware asset={asset} canEdit apps={[]} active={false} />)
    await new Promise((r) => setTimeout(r, 20))
    expect(api.installedSoftware).not.toHaveBeenCalled()
  })

  it('searches, previews on the server, and installs exactly the preview', async () => {
    api.installedSoftware.mockResolvedValue(installed)
    api.softwareRuns.mockResolvedValue({ runs: [], needsApproval: false })
    api.searchSoftware.mockResolvedValue({ state: 'ok', installed: [], found: [
      { name: 'redis-tools', version: '5:7.0.15-1build2', origin: 'noble/universe', summary: 'Redis client' }] })
    api.previewSoftware.mockResolvedValue(previewOf({}))
    api.startSoftwareRun.mockResolvedValue(runOf({ state: 'queued' }))
    renderWith(<InstalledSoftware asset={asset} canEdit apps={[]} active />)
    fireEvent.click(await screen.findByRole('button', { name: /Install…/ }))
    const drawer = await screen.findByRole('dialog')
    fireEvent.change(within(drawer).getByLabelText('Search packages'), { target: { value: 'redis' } })
    fireEvent.click(await within(drawer).findByText('redis-tools', {}, { timeout: 2000 }))
    await waitFor(() => expect(api.previewSoftware).toHaveBeenCalledWith('a1',
      { action: 'install', items: [{ name: 'redis-tools' }], startServices: true }))
    expect(await within(drawer).findByText('libjemalloc2')).toBeTruthy()
    expect(within(drawer).getByText(/Nothing removed/)).toBeTruthy()
    fireEvent.click(within(drawer).getByRole('button', { name: 'Install' }))
    await waitFor(() => expect(api.startSoftwareRun).toHaveBeenCalledWith('a1', 'p1'))
  })

  it('cannot go ahead with a blocked preview', async () => {
    api.installedSoftware.mockResolvedValue(installed)
    api.softwareRuns.mockResolvedValue({ runs: [], needsApproval: false })
    api.previewSoftware.mockResolvedValue(previewOf({
      id: 'p2', action: 'remove', items: [{ name: 'nginx' }],
      blocked: 'Removing nginx would also remove openssh-server, which Argus reaches the server through.',
      result: { ...previewOf({}).result, action: 'remove', changes: [] },
    }))
    renderWith(<InstalledSoftware asset={asset} canEdit apps={[]} active />)
    fireEvent.click(await screen.findByLabelText('Actions for nginx'))
    fireEvent.click(await screen.findByText('Remove…'))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/would also remove openssh-server/)).toBeTruthy()
    expect((within(dialog).getByRole('button', { name: 'Remove' }) as HTMLButtonElement).disabled).toBe(true)
  })

  it('removes only once the server name is typed, warning what stops', async () => {
    api.installedSoftware.mockResolvedValue(installed)
    api.softwareRuns.mockResolvedValue({ runs: [], needsApproval: true })
    api.previewSoftware.mockResolvedValue(previewOf({
      id: 'p3', action: 'remove', items: [{ name: 'nginx' }], needsApproval: true,
      result: { ...previewOf({}).result, action: 'remove', purge_allowed: true,
        changes: [{ kind: 'remove', name: 'nginx', from: '1.24.0-2ubuntu7.2', asked: true },
          { kind: 'remove', name: 'nginx-common', from: '1.24.0-2ubuntu7.2' }] },
    }))
    api.startSoftwareRun.mockResolvedValue(runOf({ state: 'awaiting_approval', mode: 'remove' }))
    renderWith(<InstalledSoftware asset={asset} canEdit apps={[nginxApp]} active />)
    fireEvent.click(await screen.findByLabelText('Actions for nginx'))
    fireEvent.click(await screen.findByText('Remove…'))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText('nginx is running')).toBeTruthy()
    expect(within(dialog).getByText('nginx-common')).toBeTruthy()
    const go = within(dialog).getByRole('button', { name: 'Ask to remove' }) as HTMLButtonElement
    expect(go.disabled).toBe(true)
    fireEvent.change(within(dialog).getByLabelText('Type the server name to confirm'), { target: { value: 'web-01' } })
    expect(go.disabled).toBe(false)
    fireEvent.click(go)
    await waitFor(() => expect(api.startSoftwareRun).toHaveBeenCalledWith('a1', 'p3'))
  })

  it('offers to undo a finished install, and approval to another admin', async () => {
    api.installedSoftware.mockResolvedValue(installed)
    api.softwareRuns.mockResolvedValue({ needsApproval: true, runs: [
      runOf({ id: 'r2', state: 'awaiting_approval', mode: 'remove', items: [{ name: 'htop' }] }),
      runOf({ id: 'r1', items: [{ name: 'redis-server' }], upgraded: [{ name: 'redis-server', to: '7.0.15' }],
        started: ['redis-server.service'] }),
    ] })
    api.undoSoftwareRun.mockResolvedValue(previewOf({ id: 'p4', action: 'remove', items: [{ name: 'redis-server' }], undoOf: 'r1',
      result: { ...previewOf({}).result, action: 'remove', changes: [{ kind: 'remove', name: 'redis-server', from: '7.0.15', asked: true }] } }))
    renderWith(<InstalledSoftware asset={asset} canEdit apps={[]} active />)
    expect(await screen.findByRole('button', { name: 'Approve' })).toBeTruthy()
    expect(screen.getByText('Started redis-server.service')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Remove what this installed' }))
    await waitFor(() => expect(api.undoSoftwareRun).toHaveBeenCalledWith('r1'))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/Remove what that installed on web-01/)).toBeTruthy()
  })
})
