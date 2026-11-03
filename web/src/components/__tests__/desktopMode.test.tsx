import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AssetForm } from '~/components/AssetForm'
import { api } from '~/lib/api'
import { assets } from '~/lib/seed'
import { renderWithProviders } from '~/test/render'

/**
 * How a Windows server's desktop opens is set on its form, and what is
 * picked is what is saved: a control that shows "Screen sharing" while the
 * server stays on Remote Desktop is the failure this product cannot have.
 */

const rdp = assets.find((a) => a.protocol === 'rdp')!

afterEach(() => {
  vi.restoreAllMocks()
})

describe('the desktop setting', () => {
  it('saves screen sharing for a Windows server', async () => {
    const update = vi.spyOn(api, 'updateAsset').mockResolvedValue({ ...rdp, desktopMode: 'screen' })
    renderWithProviders(<AssetForm opened onClose={() => {}} asset={{ ...rdp, desktopMode: 'rdp' }} />)

    expect(await screen.findByText(/signs in through the server.s own remote desktop/i)).toBeInTheDocument()
    fireEvent.click(screen.getByText('Screen sharing (no RDP)'))
    expect(screen.getByText(/anyone in front of the server sees what is done/i)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(update).toHaveBeenCalled())
    expect(update.mock.calls[0]![1].desktopMode).toBe('screen')
  })

  it('is not offered for an SSH server', async () => {
    const ssh = assets.find((a) => a.protocol !== 'rdp')!
    renderWithProviders(<AssetForm opened onClose={() => {}} asset={ssh} />)
    expect(await screen.findByText('Login accounts')).toBeInTheDocument()
    expect(screen.queryByText('Screen sharing (no RDP)')).not.toBeInTheDocument()
  })
})
