import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { AppsTab } from '~/components/server/AppsTab'
import { assets } from '~/lib/seed'
import { renderWithProviders } from '~/test/render'
import type { Asset } from '~/types/domain'

/**
 * A device's "what runs here" is its installed apps, not a server's databases
 * and ports. The fixture gives the dev build a list with no phone; here it
 * stands in for a real report.
 */

const phone = assets.find((a) => a.kind === 'android')!

afterEach(() => vi.restoreAllMocks())

describe('the Apps tab for a device', () => {
  it('lists installed apps with their package and source, and filters them', async () => {
    renderWithProviders(<AppsTab asset={{ ...phone, agentState: 'healthy' } as Asset} canEdit={false} />)

    expect(await screen.findByText('Chrome')).toBeInTheDocument()
    expect(screen.getByText('com.android.chrome')).toBeInTheDocument()
    // A sideloaded app is flagged.
    expect(screen.getByText('Northwind Field')).toBeInTheDocument()

    // Filtering to Preinstalled drops the Play and sideloaded apps.
    fireEvent.click(screen.getByRole('radio', { name: 'Preinstalled' }))
    await waitFor(() => expect(screen.queryByText('Chrome')).not.toBeInTheDocument())
    expect(screen.getByText('Settings')).toBeInTheDocument()
  })

  it('searches by label or package', async () => {
    renderWithProviders(<AppsTab asset={{ ...phone, agentState: 'healthy' } as Asset} canEdit={false} />)
    await screen.findByText('Chrome')
    fireEvent.change(screen.getByPlaceholderText('Filter apps'), { target: { value: 'gmail' } })
    await waitFor(() => expect(screen.getByText('Gmail')).toBeInTheDocument())
    expect(screen.queryByText('Chrome')).not.toBeInTheDocument()
  })
})
