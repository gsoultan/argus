import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AssetForm, hostnameNote } from '~/components/AssetForm'
import { api } from '~/lib/api'
import { assets } from '~/lib/seed'
import { renderWithProviders } from '~/test/render'

/**
 * Renaming a server from its form. The new name is what is saved, and the
 * form says what a rename keeps: the agent goes on by the name it was
 * installed under, so the administrator is not left guessing whether the
 * server is about to drop off.
 */

const ssh = assets.find((a) => a.protocol !== 'rdp' && a.kind !== 'android')!
const withAgent = { ...ssh, agentState: 'healthy' as const, tunnelConnected: true, agentHostname: undefined }

afterEach(() => {
  vi.restoreAllMocks()
})

describe('renaming a server', () => {
  it('saves the new hostname and says its agent keeps its name', async () => {
    const update = vi.spyOn(api, 'updateAsset').mockResolvedValue({ ...withAgent, hostname: 'db-07.corp.example' })
    renderWithProviders(<AssetForm opened onClose={() => {}} asset={withAgent} />)

    const field = await screen.findByLabelText(/hostname/i)
    fireEvent.change(field, { target: { value: 'db-07.corp.example' } })
    expect(screen.getByText(new RegExp(`agent still goes by ${withAgent.hostname.replace(/\./g, '\\.')}`)))
      .toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))

    await waitFor(() => expect(update).toHaveBeenCalled())
    expect(update.mock.calls[0]![1].hostname).toBe('db-07.corp.example')
  })

  it('names the agent of a server already renamed', () => {
    const renamed = { ...withAgent, hostname: 'db-07.corp.example', agentHostname: 'web-01' }
    expect(hostnameNote(renamed, renamed.hostname)).toMatch(/agent goes by web-01/)
    expect(hostnameNote(renamed, 'web-01')).toMatch(/back to its agent's own name/)
  })

  it('says nothing about an agent when adding a server', () => {
    expect(hostnameNote(undefined, 'new.corp.example')).not.toMatch(/agent/)
  })
})
