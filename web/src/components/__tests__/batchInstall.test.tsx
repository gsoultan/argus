import { fireEvent, render, screen, waitFor, within } from '@testing-library/react'
import { MantineProvider } from '@mantine/core'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { BatchInstall, batchOrder, fitsBatch } from '~/components/BatchInstall'
import { assetsQuery } from '~/lib/queries'
import type { SoftwareFile } from '~/lib/softwareApi'
import { theme } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * A library installer on many servers: only the servers it is for are
 * offered, production goes last, and the ones left out are named.
 */

const api = vi.hoisted(() => ({ startSoftwareBatch: vi.fn() }))

vi.mock('~/lib/softwareApi', async (orig) => ({
  ...(await orig<typeof import('~/lib/softwareApi')>()),
  ...api,
}))

const msi: SoftwareFile = {
  id: 'f9', platform: 'windows', kind: 'msi', name: 'Field Reports', fileName: 'fr.msi', sha256: 'c'.repeat(64),
  size: 1 << 20, note: '', createdBy: 'ada@example.com', createdAt: new Date().toISOString(), windows: { args: [] },
}

const fleet = [
  { id: 'p', hostname: 'win-prod', protocol: 'rdp', kind: 'server', tags: ['production'] },
  { id: 't', hostname: 'win-test', protocol: 'rdp', kind: 'server', tags: [] },
  { id: 'l', hostname: 'lin-01', protocol: 'ssh', kind: 'server', tags: [] },
  { id: 'd', hostname: 'pixel', protocol: 'ssh', kind: 'android', tags: [] },
] as unknown as Asset[]

afterEach(() => vi.clearAllMocks())

describe('BatchInstall', () => {
  it('offers what fits and orders production last', () => {
    expect(fleet.filter((a) => fitsBatch({ kind: 'file', file: msi }, a)).map((a) => a.id)).toEqual(['p', 't'])
    expect(fleet.filter((a) => fitsBatch({ kind: 'file', file: { ...msi, platform: 'android' } }, a)).map((a) => a.id)).toEqual(['d'])
    expect(batchOrder(fleet.slice(0, 2)).map((a) => a.hostname)).toEqual(['win-test', 'win-prod'])
  })

  it('starts one batch and names who was left out', async () => {
    api.startSoftwareBatch.mockResolvedValue({
      runs: [{ id: 'r1', assetHostname: 'win-test' }, { id: 'r2', assetHostname: 'win-prod' }],
      skipped: [{ assetId: 'x', hostname: 'win-old', reason: 'its agent is too old to install software' }],
    })
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false, staleTime: Infinity } } })
    qc.setQueryData(assetsQuery().queryKey, fleet)
    render(
      <QueryClientProvider client={qc}><MantineProvider theme={theme}>
        <BatchInstall target={{ kind: 'file', file: msi }} onClose={() => {}} />
      </MantineProvider></QueryClientProvider>,
    )
    const dialog = await screen.findByRole('dialog')
    const pick = within(dialog).getAllByLabelText('Servers')[0]!
    fireEvent.focus(pick)
    fireEvent.click(pick)
    expect(screen.queryByRole('option', { name: 'lin-01', hidden: true })).toBeNull()
    fireEvent.click(await screen.findByRole('option', { name: 'win-prod (production)', hidden: true }))
    fireEvent.click(await screen.findByRole('option', { name: 'win-test', hidden: true }))
    expect(within(dialog).getByText('1. win-test')).toBeTruthy()
    fireEvent.click(within(dialog).getByRole('button', { name: 'Install on 2 servers' }))
    await waitFor(() => expect(api.startSoftwareBatch).toHaveBeenCalledWith({ kind: 'file', fileId: 'f9', assetIds: ['t', 'p'] }))
    expect(await within(dialog).findByText('2 asked for, one at a time')).toBeTruthy()
    expect(within(dialog).getByText(/its agent is too old/)).toBeTruthy()
  })
})
