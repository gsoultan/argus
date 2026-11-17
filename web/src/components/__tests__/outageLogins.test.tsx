import { fireEvent, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { OutageLoginsCard } from '~/components/OutageLogins'
import { readinessChecks, type ReadinessView } from '~/lib/controlReadiness'
import { checked, outageAfterLabel, outageGroupsError } from '~/lib/outageLogins'
import { renderWithProviders } from '~/test/render'

const live = vi.hoisted(() => ({ get: vi.fn(), send: vi.fn(), isConfigured: () => true }))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  ...live,
}))

/** Settings › Logins › While Argus cannot be reached. */

describe('the group list', () => {
  it('needs at least one real group name', () => {
    expect(outageGroupsError([])).toMatch(/at least one/)
    expect(outageGroupsError(['sudo', 'rm -rf'])).toMatch(/not a group name/)
    expect(outageGroupsError(['sudo', 'wheel', 'on-call_2'])).toBeNull()
  })
  it('says the wait in words', () => {
    expect(outageAfterLabel(60)).toBe('1 hour')
    expect(outageAfterLabel(15)).toBe('15 minutes')
  })
})

describe('the card', () => {
  it('saves the wait and groups, and warns once it is on', async () => {
    live.get.mockResolvedValue({ afterMinutes: 0, groups: ['sudo', 'wheel', 'admin'] })
    live.send.mockResolvedValue({ afterMinutes: 15, groups: ['sudo', 'wheel', 'admin'] })
    renderWithProviders(<OutageLoginsCard canEdit />)
    await screen.findByText('Let in after')
    expect(screen.queryByText(/cut a server off from Argus/)).toBeNull()
    fireEvent.click(screen.getByText('15 min'))
    expect(screen.getByText(/cut a server off from Argus/)).toBeInTheDocument()
    fireEvent.click(screen.getByRole('button', { name: 'Save' }))
    await waitFor(() => expect(live.send).toHaveBeenCalledWith('PUT', '/api/v1/policy/outage-logins',
      { afterMinutes: 15, groups: ['sudo', 'wheel', 'admin'] }))
  })
})

describe('the readiness page', () => {
  const v = (letIn: number): ReadinessView => ({
    outsideCheck: { configured: true }, servers: [], running: 1,
    backups: { installed: true, timerOn: true, lastOk: true },
    directSsh: { refusing: 10, total: 12, letIn },
    confirmations: { backups_off_host: null, key_off_host: null, provider_consoles: null }, confirmDays: 90,
  })
  const ssh = (x: ReadinessView) => readinessChecks(x, Date.now()).find((l) => l.id === 'ssh')!
  it('is fine when every refusing server lets admins in', () => {
    expect(ssh(v(10)).tone).toBe('ok')
  })
  it('counts the servers that still refuse everyone', () => {
    expect(ssh(v(7)).tone).toBe('warn')
    expect(ssh(v(7)).title).toMatch(/^3 of 12/)
  })
})

describe('the answer the card draws from', () => {
  it('fails the query, not the page, when it is not the setting', () => {
    for (const bad of [[], {}, null, { afterMinutes: 15 }]) {
      expect(() => checked(bad as never)).toThrow(/while it is down/)
    }
    const ok = { afterMinutes: 0, groups: ['sudo'] }
    expect(checked(ok)).toBe(ok)
  })
})
