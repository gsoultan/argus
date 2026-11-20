import { describe, expect, it, vi } from 'vitest'
import { fireEvent, screen, waitFor } from '@testing-library/react'
import { renderWithProviders as ui } from '~/test/render'
import type { Asset } from '~/types/domain'

/*
Create new on a SQL Server: the dialog offers the server's collations and
recovery models, and the preview it shows is the control plane's dry run of
the request with those fields.
*/

const api = vi.hoisted(() => ({ createOptions: vi.fn(), createOnServer: vi.fn() }))
vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  isConfigured: () => true,
  databasesOnAsset: async () => [],
}))
vi.mock('~/lib/createApi', async (orig) => ({ ...(await orig<typeof import('~/lib/createApi')>()), ...api }))

const { CreateOnServer } = await import('~/components/CreateOnServer')

describe('Create new on SQL Server', () => {
  it('offers collations and recovery models, and sends them', async () => {
    api.createOptions.mockResolvedValue({ state: 'ok', options: {
      version: '16.0.4135.4', defaultCollation: 'SQL_Latin1_General_CP1_CI_AS', defaultRecovery: 'FULL',
      collations: ['Latin1_General_100_CI_AS_SC_UTF8', 'SQL_Latin1_General_CP1_CI_AS'], taken: ['shop'],
    } })
    api.createOnServer.mockResolvedValue({ state: 'ok', statements: ['CREATE DATABASE [crm]'], steps: [] })
    const asset = { id: 'a1', hostname: 'db1.corp', address: '10.0.0.5', port: 22 } as Asset
    ui(<CreateOnServer asset={asset} engine="mssql" onClose={() => {}} onAddExisting={() => {}} />)
    expect(await screen.findByText('Recovery model')).toBeTruthy()
    await waitFor(() => expect(screen.getByPlaceholderText('SQL_Latin1_General_CP1_CI_AS (server)')).toBeTruthy())
    expect(screen.getByDisplayValue('As model (FULL)')).toBeTruthy()
    fireEvent.change(screen.getByPlaceholderText('invoices'), { target: { value: 'crm' } })
    await waitFor(() => expect(api.createOnServer).toHaveBeenCalledWith('a1',
      expect.objectContaining({ engine: 'mssql', name: 'crm', dryRun: true, mssql: expect.any(Object) })))
    expect(api.createOptions).toHaveBeenCalledWith('a1', expect.objectContaining({ engine: 'mssql' }))
  })
})
