import { screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { SettingsTab } from '~/components/server/SettingsTab'
import { assets } from '~/lib/seed'
import { renderWithProviders } from '~/test/render'

/**
 * argus-control's own server. It is looked after through the helper its
 * package installs, so its Settings explain that instead of offering a
 * Remove the control plane would refuse, and say plainly that plain SSH to
 * it is not gated: an operator must not believe a way in is recorded when it
 * is not.
 */

const server = assets.find((a) => a.protocol !== 'rdp' && a.kind !== 'android')!

describe('the control server', () => {
  it('has no Remove button, says why, and says plain SSH is not gated', async () => {
    renderWithProviders(<SettingsTab asset={{ ...server, controlHost: true }} canEdit onEdit={() => {}} />)
    expect(await screen.findByTestId('control-host-remove')).toHaveTextContent('control_host: false')
    expect(screen.queryByRole('button', { name: /^remove$/i })).toBeNull()
    expect(screen.getByText(/not gated or recorded by Argus/)).toBeInTheDocument()
  })

  it('leaves every other server removable', async () => {
    renderWithProviders(<SettingsTab asset={{ ...server, controlHost: false }} canEdit onEdit={() => {}} />)
    expect(await screen.findByRole('button', { name: /^remove$/i })).toBeInTheDocument()
    expect(screen.queryByTestId('control-host-remove')).toBeNull()
    expect(screen.queryByText(/not gated or recorded by Argus/)).toBeNull()
  })
})
