import { screen } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { api } from '~/lib/api'
import { currentUser } from '~/lib/seed'
import type { DiscoveredHost } from '~/types/domain'
import { renderRoute } from '~/test/renderRoute'

/**
 * A device whose app checked in before anyone finished adding it: the
 * Servers page offers to finish it as a device, told from a server by the
 * OS the app reports, so the last step asks how it is controlled rather
 * than which accounts it has.
 */

afterEach(() => {
  vi.restoreAllMocks()
})

describe('a device waiting to be finished', () => {
  it('is finished as a device, not a server, from the Servers page', async () => {
    vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role: 'admin' })
    const waiting: DiscoveredHost[] = [{
      hostname: 'rina-pixel-8', os: 'Android 14', addresses: [], sshPorts: [], accounts: [],
      version: '1.0.0', firstSeenAt: '2026-01-01T00:00:00Z', lastSeenAt: '2026-01-01T00:00:00Z', state: 'unreviewed',
    }, {
      hostname: 'web-02', os: 'Ubuntu 24.04', addresses: [], sshPorts: [22], accounts: [],
      version: '1.0.0', firstSeenAt: '2026-01-01T00:00:00Z', lastSeenAt: '2026-01-01T00:00:00Z', state: 'unreviewed',
    }]
    await renderRoute('/assets', (qc) => qc.setQueryData(['discovered', 'unreviewed'], waiting))
    const links = await screen.findAllByRole('link', { name: /finish adding/i })
    expect(links.map((l) => l.getAttribute('href'))).toEqual([
      '/assets/add?host=rina-pixel-8&kind=android', '/assets/add?host=web-02',
    ])
  })
})

