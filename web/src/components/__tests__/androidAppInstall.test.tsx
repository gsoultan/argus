import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AppsTab } from '~/components/server/AppsTab'
import type { PackageRun } from '~/lib/live'
import { assets } from '~/lib/seed'
import type { SoftwareFile } from '~/lib/softwareApi'
import { renderWithProviders } from '~/test/render'
import type { Asset } from '~/types/domain'

/**
 * Installing an app from the library on a device, and uninstalling,
 * disabling and restoring from its Apps tab: what is offered for which app,
 * what the person is told before it is sent, and the wait while it goes.
 */

const api = vi.hoisted(() => ({
  softwareRuns: vi.fn(),
  softwareFiles: vi.fn(),
  runAndroidApp: vi.fn(),
}))

vi.mock('~/lib/live', async (orig) => {
  const m = await orig<typeof import('~/lib/live')>()
  // The device's own report comes from the fixtures.
  const offline = () => Promise.reject(new Error('offline'))
  return { ...m, isConfigured: () => true, live: { ...m.live, androidApps: offline, system: offline } }
})

vi.mock('~/lib/softwareApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/softwareApi')>()),
  ...api,
}))

const phone = { ...assets.find((a) => a.kind === 'android')!, agentState: 'healthy' } as Asset

const survey: SoftwareFile = {
  id: 'f1', platform: 'android', kind: 'apk', name: 'Field Survey', fileName: 'survey.apk', sha256: 'a'.repeat(64),
  size: 12 << 20, note: '', createdBy: 'ada@example.com', createdAt: new Date().toISOString(),
  app: { package: 'com.impack.survey', versionCode: 240, versionName: '2.4.0', label: 'Field Survey', minSdk: 29, signer: 'ab12cd34'.repeat(8), scheme: 'v2' },
}

const runOf = (over: Partial<PackageRun>): PackageRun => ({
  id: 'r1', assetId: phone.id, mode: 'app_uninstall', state: 'queued', requestedBy: 'ada@example.com',
  requestedAt: new Date().toISOString(), upgraded: [], items: [{ name: 'id.northwind.field' }], ...over,
} as PackageRun)

afterEach(() => vi.clearAllMocks())

describe('apps on a device', () => {
  it('offers uninstall for an app someone installed, and keeps Argus and Settings', async () => {
    api.softwareRuns.mockResolvedValue({ runs: [] })
    renderWithProviders(<AppsTab asset={{ ...phone, controlMode: 'attended' } as Asset} canEdit />)
    await screen.findByText('Northwind Field')
    // Argus and Settings are never offered.
    expect(screen.getAllByText('Kept').length).toBe(2)
    expect(screen.queryByRole('button', { name: 'Actions for Settings' })).toBeNull()
    // A preinstalled app cannot be disabled with the attended engine.
    expect(screen.queryByRole('button', { name: 'Actions for Phone' })).toBeNull()

    api.runAndroidApp.mockResolvedValue(runOf({}))
    fireEvent.click(screen.getByRole('button', { name: 'Actions for Northwind Field' }))
    fireEvent.click(await screen.findByRole('menuitem', { name: 'Uninstall…' }))
    const dialog = await screen.findByRole('dialog')
    expect(within(dialog).getByText(/Android asks the person holding the device to confirm/)).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Uninstall' }))
    await waitFor(() => expect(api.runAndroidApp).toHaveBeenCalledWith(phone.id, { action: 'uninstall', package: 'id.northwind.field' }))
  })

  it('disables and restores preinstalled apps with the unattended engine', async () => {
    api.softwareRuns.mockResolvedValue({ runs: [] })
    renderWithProviders(<AppsTab asset={{ ...phone, controlMode: 'unattended' } as Asset} canEdit />)
    await screen.findByText('Calculator')
    expect(screen.getByText('Disabled')).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Actions for Calculator' }))
    expect(await screen.findByRole('menuitem', { name: 'Restore…' })).toBeTruthy()
    fireEvent.click(screen.getByRole('button', { name: 'Actions for Phone' }))
    expect(await screen.findByRole('menuitem', { name: 'Disable…' })).toBeTruthy()
  })

  it('installs a library APK, showing what the device will check', async () => {
    api.softwareRuns.mockResolvedValue({ runs: [] })
    api.softwareFiles.mockResolvedValue([survey])
    api.runAndroidApp.mockResolvedValue(runOf({ mode: 'app_install', items: [{ name: 'com.impack.survey', version: '2.4.0' }] }))
    renderWithProviders(<AppsTab asset={phone} canEdit />)
    fireEvent.click(await screen.findByRole('button', { name: 'Install app…' }))
    const dialog = await screen.findByRole('dialog')
    await waitFor(() => expect(api.softwareFiles).toHaveBeenCalled())
    const pick = within(dialog).getByLabelText('App')
    fireEvent.focus(pick)
    fireEvent.change(pick, { target: { value: 'Field' } })
    fireEvent.click(await screen.findByRole('option', { name: 'Field Survey 2.4.0', hidden: true }))
    expect(await within(dialog).findByText('com.impack.survey')).toBeTruthy()
    expect(within(dialog).getByText('Not on the device yet.')).toBeTruthy()
    expect(within(dialog).getByText(/ab12cd34…cd34/)).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Install' }))
    await waitFor(() => expect(api.runAndroidApp).toHaveBeenCalledWith(phone.id, { action: 'install', fileId: 'f1' }))
  })

  it('waits for one change at a time', async () => {
    api.softwareRuns.mockResolvedValue({ runs: [runOf({ state: 'running', sentAt: new Date().toISOString(), message: 'Android asked the person holding the device to confirm uninstalling id.northwind.field' })] })
    renderWithProviders(<AppsTab asset={phone} canEdit />)
    expect(await screen.findByText('ada@example.com asked to uninstall id.northwind.field')).toBeTruthy()
    expect(screen.getByRole('button', { name: 'Install app…' })).toBeDisabled()
    expect(screen.queryByRole('button', { name: 'Actions for Northwind Field' })).toBeNull()
  })

  it('offers nothing to someone who cannot change the device', async () => {
    api.softwareRuns.mockResolvedValue({ runs: [] })
    renderWithProviders(<AppsTab asset={phone} canEdit={false} />)
    await screen.findByText('Northwind Field')
    expect(screen.queryByRole('button', { name: 'Install app…' })).toBeNull()
    expect(screen.queryByText('Kept')).toBeNull()
  })
})
