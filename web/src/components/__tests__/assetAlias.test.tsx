import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AssetForm, hostnameError } from '~/components/AssetForm'
import { api } from '~/lib/api'
import { assetHostnameAside, assetName, assetShortName } from '~/lib/assetType'
import { assets } from '~/lib/seed'
import { renderWithProviders } from '~/test/render'

/**
 * An asset's alias: the name people know it by, shown in place of the
 * hostname, which has to stay a DNS name. Before it, "Payroll database" could
 * not be given to a server at all, and a server with no login accounts could
 * not be edited: Save stayed grey with nothing saying why.
 */

const ssh = assets.find((a) => a.protocol !== 'rdp' && a.kind !== 'android')!

afterEach(() => {
  vi.restoreAllMocks()
})

describe('an asset alias', () => {
  it('is shown in place of the hostname, which is kept beside it', () => {
    const a = { ...ssh, hostname: 'pay-01.corp.example', alias: 'Payroll database' }
    expect(assetName(a)).toBe('Payroll database')
    expect(assetShortName(a)).toBe('Payroll database')
    expect(assetHostnameAside(a)).toBe('pay-01.corp.example')

    const plain = { ...a, alias: '' }
    expect(assetName(plain)).toBe('pay-01.corp.example')
    expect(assetShortName(plain)).toBe('pay-01')
    expect(assetHostnameAside(plain)).toBeUndefined()
  })

  it('is saved from the edit form without touching the hostname', async () => {
    const update = vi.spyOn(api, 'updateAsset').mockResolvedValue({ ...ssh, alias: 'Payroll database' })
    renderWithProviders(<AssetForm opened onClose={() => {}} asset={ssh} />)

    fireEvent.change(await screen.findByLabelText(/^alias/i), { target: { value: 'Payroll database' } })
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(update).toHaveBeenCalled())
    expect(update.mock.calls[0]![1].alias).toBe('Payroll database')
    expect(update.mock.calls[0]![1].hostname).toBe(ssh.hostname)
  })

  it('lets a server with no login accounts be edited', async () => {
    const bare = { ...ssh, principals: [] }
    const update = vi.spyOn(api, 'updateAsset').mockResolvedValue(bare)
    renderWithProviders(<AssetForm opened onClose={() => {}} asset={bare} />)

    fireEvent.change(await screen.findByLabelText(/hostname/i), { target: { value: 'renamed-01.corp.example' } })
    const save = screen.getByRole('button', { name: 'Save' })
    expect(save).toBeEnabled()
    fireEvent.click(save)
    await waitFor(() => expect(update).toHaveBeenCalled())
  })

  it('says why a hostname with spaces is refused, and points at the alias', async () => {
    expect(hostnameError('Payroll database')).toMatch(/alias/)
    expect(hostnameError('pay_01')).toMatch(/DNS name/)
    expect(hostnameError('pay-01.corp.example')).toBeUndefined()

    renderWithProviders(<AssetForm opened onClose={() => {}} asset={ssh} />)
    fireEvent.change(await screen.findByLabelText(/hostname/i), { target: { value: 'Payroll database' } })
    expect(screen.getByText(/has no spaces/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled()
  })
})
