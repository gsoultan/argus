import { fireEvent, screen, waitFor } from '@testing-library/react'
import { afterEach, describe, expect, it, vi } from 'vitest'
import { renderRoute } from '~/test/renderRoute'
import { api } from '~/lib/api'
import { alertLinkKeys } from '~/lib/alerts'
import { currentUser } from '~/lib/seed'

/**
 * Opening an alert from the bell while already on the page it links to.
 *
 * That is a navigation to the same route: nothing remounts and the loader hands
 * back what is cached, so Approvals kept showing the queue from before the
 * alert until someone reloaded the page.
 */

afterEach(() => {
  vi.restoreAllMocks()
})

describe('an alert link to the page on screen', () => {
  it('fetches the approvals queue again', async () => {
    vi.spyOn(api, 'me').mockResolvedValue({ ...currentUser, role: 'admin' })
    vi.spyOn(api, 'directLogins').mockResolvedValue([])
    const requests = vi.spyOn(api, 'requests')

    await renderRoute('/approvals')
    await screen.findByRole('heading', { name: 'Approvals' })
    await waitFor(() => expect(requests).toHaveBeenCalled())
    const before = requests.mock.calls.length

    fireEvent.click(await screen.findByRole('button', { name: /^Alerts/ }))
    fireEvent.click(await screen.findByText('rina@northwind.id asks for root on db-02'))

    await waitFor(() => expect(requests.mock.calls.length).toBeGreaterThan(before))
  })

  it('names the queries behind each page an alert links to', () => {
    expect(alertLinkKeys('/approvals')).toEqual([['requests'], ['direct-logins']])
    expect(alertLinkKeys('/approvals?ask=web-01')).toEqual([['requests'], ['direct-logins']])
    expect(alertLinkKeys('/audit')).toEqual([['audit']])
    expect(alertLinkKeys('/databases/db_1?panel=changes')).toEqual([['databases'], ['database', 'db_1']])
    // Not a prefix match on the name: /approvalsx is not Approvals.
    expect(alertLinkKeys('/approvalsx')).toEqual([])
    expect(alertLinkKeys('/activity')).toEqual([])
  })
})
