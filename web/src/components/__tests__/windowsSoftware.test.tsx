import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { SoftwareLibrary } from '~/components/SoftwareLibrary'
import { InstalledSoftware } from '~/components/server/InstalledSoftware'
import type { PackageRun } from '~/lib/live'
import { softwareRunText, type SoftwareAnswer, type SoftwareFile } from '~/lib/softwareApi'
import { theme } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * A Windows server's programs and installing on it: programs by name, no
 * repository search, MSI and EXE files from the library, and the library
 * asking for an EXE's quiet switches before it keeps one.
 */

const api = vi.hoisted(() => ({
  installedSoftware: vi.fn(),
  previewSoftware: vi.fn(),
  softwareRuns: vi.fn(),
  softwareFiles: vi.fn(),
  runSoftwareFile: vi.fn(),
  uploadSoftwareFile: vi.fn(),
  scripts: vi.fn(() => Promise.resolve([])),
  scriptRules: vi.fn(() => Promise.resolve([])),
}))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
}))

vi.mock('~/lib/softwareApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/softwareApi')>()),
  ...api,
}))

const server = { id: 'w1', hostname: 'reports-01.example', protocol: 'rdp', tags: [] } as unknown as Asset

const programs: SoftwareAnswer = {
  state: 'ok', manager: 'windows', total: 2, manual: 2, found: [],
  installed: [
    { name: 'msi-8a69d345-d564-463c-aff1-a69d9e530f96', label: 'Acme Reporter', version: '4.2.0', origin: 'Acme', manual: true },
    { name: 'app-0011223344556677', label: 'Argus Agent', version: '1.0', manual: true, protected: 'the Argus agent' },
  ],
}

const msi: SoftwareFile = {
  id: 'f9', platform: 'windows', kind: 'msi', name: 'Field Reports', fileName: 'field-reports-1.0.msi', sha256: 'c'.repeat(64),
  size: 30 << 20, note: '', createdBy: 'ada@example.com', createdAt: new Date().toISOString(),
  windows: { args: ['ALLUSERS=1'] },
}

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

describe('software on a Windows server', () => {
  it('lists programs by name and removes by id', async () => {
    api.installedSoftware.mockResolvedValue(programs)
    api.softwareRuns.mockResolvedValue({ runs: [] })
    api.previewSoftware.mockResolvedValue({ id: 'p1', assetId: 'w1', action: 'remove', items: [], purge: false, startServices: false,
      result: { action: 'remove', changes: [], purge_allowed: false, hash: 'a'.repeat(64) }, needsApproval: false,
      createdBy: 'grace@example.com', createdAt: new Date().toISOString(), expiresAt: new Date(Date.now() + 60_000).toISOString() })
    renderWith(<InstalledSoftware asset={server} canEdit apps={[]} active />)
    expect(await screen.findByText('Acme Reporter')).toBeTruthy()
    expect(screen.getByText('Windows · 2 programs, as Apps lists them')).toBeTruthy()
    expect(screen.queryByText('Show libraries and dependencies')).toBeNull()
    // Argus itself offers nothing.
    expect(screen.queryByRole('button', { name: 'Actions for Argus Agent' })).toBeNull()
    fireEvent.click(screen.getByRole('button', { name: 'Actions for Acme Reporter' }))
    expect(screen.queryByRole('menuitem', { name: 'Reinstall' })).toBeNull()
    fireEvent.click(await screen.findByRole('menuitem', { name: 'Remove…' }))
    await waitFor(() => expect(api.previewSoftware).toHaveBeenCalledWith('w1', {
      action: 'remove', items: [{ name: 'msi-8a69d345-d564-463c-aff1-a69d9e530f96', label: 'Acme Reporter' }],
    }))
  })

  it('installs from a library file, not a repository', async () => {
    api.installedSoftware.mockResolvedValue(programs)
    api.softwareRuns.mockResolvedValue({ runs: [] })
    api.softwareFiles.mockResolvedValue([msi])
    api.runSoftwareFile.mockResolvedValue({ id: 'r1' })
    renderWith(<InstalledSoftware asset={server} canEdit apps={[]} active />)
    fireEvent.click(await screen.findByRole('button', { name: 'Install…' }))
    expect(await screen.findByText('Library file')).toBeTruthy()
    expect(screen.getByText('PowerShell script')).toBeTruthy()
    expect(screen.queryByPlaceholderText('Search packages')).toBeNull()
    await waitFor(() => expect(api.softwareFiles).toHaveBeenCalledWith('windows'))
    const pick = screen.getAllByLabelText('Installer')[0]!
    fireEvent.focus(pick)
    fireEvent.change(pick, { target: { value: 'Field' } })
    fireEvent.click(await screen.findByRole('option', { name: 'Field Reports (MSI)', hidden: true }))
    expect(await screen.findByText('msiexec /i field-reports-1.0.msi /qn /norestart ALLUSERS=1')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Install' }))
    await waitFor(() => expect(api.runSoftwareFile).toHaveBeenCalledWith('w1', 'f9'))
  })

  it('says what a file install did', () => {
    const r = { mode: 'file_install', state: 'done', softwareFileName: 'Field Reports', items: [], upgraded: [] } as unknown as PackageRun
    expect(softwareRunText(r)).toBe('installed Field Reports')
    const rm = { mode: 'remove', state: 'done', items: [{ name: 'msi-x', label: 'Acme Reporter' }], upgraded: [] } as unknown as PackageRun
    expect(softwareRunText(rm)).toContain('Acme Reporter')
  })
})

describe('the library takes Windows installers', () => {
  it('asks an EXE for its quiet switches', async () => {
    api.softwareFiles.mockResolvedValue([])
    api.uploadSoftwareFile.mockResolvedValue({ ...msi, id: 'f10', kind: 'exe', name: 'Vendor tool', fileName: 'vendor.exe', windows: { args: ['/S'], subject: 'Vendor Ltd' } })
    renderWith(<SoftwareLibrary canEdit />)
    await screen.findByRole('button', { name: 'Add a file' })
    fireEvent.change(document.querySelector('input[type=file]')!, { target: { files: [new File(['MZ'], 'vendor.exe')] } })
    const dialog = await screen.findByRole('dialog')
    const add = within(dialog).getByRole('button', { name: 'Add' })
    expect(add).toBeDisabled()
    fireEvent.change(within(dialog).getByLabelText(/Silent switches/), { target: { value: '/S' } })
    fireEvent.click(add)
    await waitFor(() => expect(api.uploadSoftwareFile).toHaveBeenCalled())
    const [file, note, opts] = api.uploadSoftwareFile.mock.calls[0]!
    expect((file as File).name).toBe('vendor.exe')
    expect(note).toBe('')
    expect(opts).toEqual({ name: 'vendor', args: '/S' })
    expect(await screen.findByText(/signed by Vendor Ltd/)).toBeTruthy()
  })
})
