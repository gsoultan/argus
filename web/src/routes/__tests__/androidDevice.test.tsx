import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AssetForm } from '~/components/AssetForm'
import { api } from '~/lib/api'
import { deviceEnrolPayload } from '~/lib/live'
import { kindOf } from '~/lib/reach'
import { isElevatedAccount, principalLabel } from '~/lib/roles'
import { assets, currentUser } from '~/lib/seed'
import { renderWithProviders } from '~/test/render'
import { renderRoute } from '~/test/renderRoute'

/**
 * An Android device in the console: added by scanning rather than pasting,
 * opened to watch or to control rather than as an account, and told apart
 * from a server wherever a server's words would be wrong.
 */

const phone = assets.find((a) => a.kind === 'android')!

afterEach(() => {
  vi.restoreAllMocks()
})

describe('what a device is', () => {
  it('is its own kind, told by the row and not by guessing from the OS', () => {
    expect(kindOf(phone)).toBe('android')
    expect(kindOf({ ...phone, kind: undefined })).toBe('windows')
  })

  it('needs an approval for control only when its settings say so, and never for watching', () => {
    expect(isElevatedAccount('control', { ...phone, controlNeedsApproval: true })).toBe(true)
    expect(isElevatedAccount('control', { ...phone, controlNeedsApproval: false })).toBe(false)
    expect(isElevatedAccount('view', { ...phone, controlNeedsApproval: true })).toBe(false)
    // A server's rule is untouched.
    expect(isElevatedAccount('root')).toBe(true)
    expect(isElevatedAccount('control')).toBe(false)
  })

  it('calls its two principals what they do', () => {
    expect(principalLabel('view', phone)).toBe('Watch only')
    expect(principalLabel('control', phone)).toBe('Control')
    expect(principalLabel('root', assets[0])).toBe('root')
  })
})

describe('the code the app scans', () => {
  it('carries the address, the one-time token and how to trust the address', () => {
    const t = { id: 'x', token: 'tok-1', expiresAt: '2030-01-01T00:00:00Z', fleetUrl: 'https://argus.example/' }
    expect(JSON.parse(deviceEnrolPayload(t))).toEqual({
      argus: 'enrol', url: 'https://argus.example', token: 'tok-1', pin: 'off',
    })
    expect(JSON.parse(deviceEnrolPayload({ ...t, pin: 'sha256:abc' })).pin).toBe('sha256:abc')
  })
})

describe('adding a device', () => {
  it('offers Android and shows a code to scan instead of a line to paste', async () => {
    await renderRoute('/assets/add')
    fireEvent.change(await screen.findByRole('textbox', { name: /name/i }), { target: { value: 'rina-pixel-8' } })
    fireEvent.click(screen.getByText('Android'))
    fireEvent.click(screen.getByRole('button', { name: /next: scan/i }))
    expect(await screen.findByRole('img', { name: /QR code for the Argus app/i })).toBeInTheDocument()
    expect(screen.getByText(/scan from rina-pixel-8/i)).toBeInTheDocument()
    expect(screen.queryByText(/curl -fsSL/)).not.toBeInTheDocument()
    expect(screen.getByText(/argus-android\.apk/)).toBeInTheDocument()
  })
})

describe('a device on its page', () => {
  it('edits how it is controlled, and saves what was picked', async () => {
    const update = vi.spyOn(api, 'updateAsset').mockResolvedValue({ ...phone, controlMode: 'unattended' })
    renderWithProviders(<AssetForm opened onClose={() => {}} asset={{ ...phone, controlNeedsApproval: false }} />)

    expect(await screen.findByText(/whoever holds the device accepts each session/i)).toBeInTheDocument()
    // Nothing a server has: no protocol, port or login accounts to edit.
    expect(screen.queryByText('Remote Desktop')).not.toBeInTheDocument()
    expect(screen.queryByText('Login accounts')).not.toBeInTheDocument()

    fireEvent.click(screen.getByText('Unattended'))
    expect(screen.getByText(/opens the device with nobody there/i)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('switch', { name: /needs an approval first/i }))
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(update).toHaveBeenCalled())
    const input = update.mock.calls[0]![1]
    expect(input.kind).toBe('android')
    expect(input.controlMode).toBe('unattended')
    expect(input.controlNeedsApproval).toBe(true)
    expect(input.principals).toEqual(['view', 'control'])
  })
})

describe('opening a device', () => {
  it('asks whether to watch or control it, with no password, and says control needs approval', async () => {
    vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role: 'operator' })
    vi.spyOn(api, 'assets').mockResolvedValue([phone])
    vi.spyOn(api, 'requests').mockResolvedValue([])
    await renderRoute(`/connect?host=${phone.hostname}&as=view`)
    expect(await screen.findByRole('radio', { name: /^Watch only/ })).toBeChecked()
    expect(screen.getByRole('radio', { name: /^Control/ })).toBeInTheDocument()
    expect(screen.getByText(/needs approval first/i)).toBeInTheDocument()
    expect(screen.queryByLabelText(/password/i)).not.toBeInTheDocument()
    expect(screen.getByRole('button', { name: /^Watch$/ })).toBeInTheDocument()
  })
})
