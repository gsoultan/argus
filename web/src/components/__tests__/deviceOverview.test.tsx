import { screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { OverviewTab } from '~/components/server/OverviewTab'
import { assets } from '~/lib/seed'
import { renderWithProviders } from '~/test/render'
import type { Asset } from '~/types/domain'

/**
 * A device's Overview says two things a server has no equal to: which engine
 * drives it, and whether wireless debugging — a shell-level door — is on.
 * Both come from what the device reports, which in these tests the fixture
 * derives from the control mode.
 */

const phone = assets.find((a) => a.kind === 'android')!

afterEach(() => vi.restoreAllMocks())

describe('a device on its Overview', () => {
  it('names the engine and warns when wireless debugging is on, for unattended', async () => {
    renderWithProviders(
      <OverviewTab asset={{ ...phone, controlMode: 'unattended', agentState: 'healthy' } as Asset} sessions={[]} onTab={() => {}} />,
    )
    expect(await screen.findByText(/Unattended \(the helper, nobody there\)/i)).toBeInTheDocument()
    await waitFor(() => expect(screen.getByText(/listens on every interface/i)).toBeInTheDocument())
  })

  it('says attended and shows no wireless-debugging warning for an attended device', async () => {
    renderWithProviders(
      <OverviewTab asset={{ ...phone, controlMode: 'attended', agentState: 'healthy' } as Asset} sessions={[]} onTab={() => {}} />,
    )
    expect(await screen.findByText(/Attended \(accepted on the device\)/i)).toBeInTheDocument()
    expect(screen.queryByText(/listens on every interface/i)).not.toBeInTheDocument()
  })
})
