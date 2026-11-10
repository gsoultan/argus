import { fireEvent, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ControlUpdatePanel } from '~/components/ControlUpdate'
import { ControlUpdatingBanner } from '~/components/ControlUpdateBanner'
import type { ControlUpdateView } from '~/lib/live'
import { renderWithProviders } from '~/test/render'

const live = vi.hoisted(() => ({ checkControlUpdate: vi.fn() }))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  ...live,
}))

const notify = vi.hoisted(() => ({ notifyOk: vi.fn(), notifyWarn: vi.fn() }))

vi.mock('~/lib/notify', async (orig) => ({
  ...(await orig<typeof import('~/lib/notify')>()),
  ...notify,
}))

/**
 * Settings › Control plane › Updates says, before anything starts, what the
 * restart would do, from what the control plane reported: which sessions end,
 * and whether going back would be clean for the data.
 */

const view = (over: Partial<ControlUpdateView> = {}): ControlUpdateView => ({
  running: '0.8.2',
  hostname: 'argus-01',
  supported: true,
  newest: { version: '0.9.0', publishedAt: '2026-11-04T09:00:00Z', page: 'https://example.com/v0.9.0' },
  newer: true,
  releaseChecks: true,
  checkedAt: '2026-11-06T07:00:00Z',
  rollback: { known: true, added: 3, breaking: [] },
  sessions: { held: 2, ending: 0, terminals: 0, desktops: 0, databases: 0 },
  update: null,
  ...over,
})

describe('Updating the control plane', () => {
  it('offers the newest release, with a clean way back and nothing cut off', () => {
    renderWithProviders(<ControlUpdatePanel v={view()} />)
    expect(screen.getByTestId('update-running').textContent).toBe('0.8.2')
    expect(screen.getByTestId('update-newest').textContent).toBe('0.9.0')
    expect(screen.getByTestId('update-rollback').getAttribute('data-tone')).toBe('ok')
    expect(screen.getByText('Rolls back cleanly')).toBeTruthy()
    expect(screen.getByTestId('update-sessions').getAttribute('data-tone')).toBe('ok')
    expect(screen.getByText(/2 terminals pause/)).toBeTruthy()
    expect((screen.getByTestId('update-start') as HTMLButtonElement).disabled).toBe(false)
  })

  it('names the sessions that would end', () => {
    renderWithProviders(<ControlUpdatePanel v={view({
      sessions: { held: 1, ending: 3, terminals: 1, desktops: 2, databases: 0 },
    })} />)
    expect(screen.getByTestId('update-sessions').getAttribute('data-tone')).toBe('warn')
    expect(screen.getAllByText(/1 terminal on an older agent, 2 desktops/).length).toBeGreaterThan(0)
    expect(screen.getByText(/Waits for the 3 sessions/)).toBeTruthy()
  })

  it('warns when going back would need the backup', () => {
    renderWithProviders(<ControlUpdatePanel v={view({
      rollback: { known: true, added: 2, breaking: [{ name: '070_drop.sql', why: 'drops users.nickname' }] },
    })} />)
    expect(screen.getByTestId('update-rollback').getAttribute('data-tone')).toBe('warn')
    expect(screen.getByText('Going back would need the backup')).toBeTruthy()
    expect(screen.getByText(/drops users.nickname/)).toBeTruthy()
  })

  it('says why an install cannot update itself, and does not offer to', () => {
    renderWithProviders(<ControlUpdatePanel v={view({ supported: false, reason: 'not run by systemd' })} />)
    expect(screen.getByTestId('update-unsupported').textContent).toContain('not run by systemd')
    expect((screen.getByTestId('update-start') as HTMLButtonElement).disabled).toBe(true)
  })

  it('says up to date and offers nothing on the newest version', () => {
    renderWithProviders(<ControlUpdatePanel v={view({ running: '0.9.0', newer: false, rollback: undefined })} />)
    expect(screen.getByText('Up to date')).toBeTruthy()
    expect(screen.queryByTestId('update-start')).toBeNull()
  })

  it('follows an update step by step, and can cancel one that waits', () => {
    renderWithProviders(<ControlUpdatePanel v={view({
      update: {
        id: 'u1', from: '0.8.2', target: '0.9.0', requested_by: 'ops@acme.co', state: 'waiting',
        message: 'waiting for 1 session that would end with the restart', at: new Date().toISOString(),
        steps: [{ state: 'waiting', message: 'waiting for 1 session that would end with the restart', at: new Date().toISOString() }],
      },
    })} />)
    expect(screen.getByTestId('update-progress').getAttribute('data-state')).toBe('waiting')
    expect(screen.getByTestId('update-cancel')).toBeTruthy()
    expect(screen.queryByTestId('update-start')).toBeNull()
  })

  it('says it went back, and to which version', () => {
    const at = new Date().toISOString()
    renderWithProviders(<ControlUpdatePanel v={view({
      update: {
        id: 'u1', from: '0.8.2', target: '0.9.0', state: 'rolled_back', at,
        message: '0.9.0 exited 3 times as it started; went back to 0.8.2',
        steps: [
          { state: 'installing', message: 'installing 0.9.0', at },
          { state: 'rolling_back', message: '0.9.0 exited 3 times as it started; going back to 0.8.2', at },
          { state: 'rolled_back', message: '0.9.0 exited 3 times as it started; went back to 0.8.2', at },
        ],
      },
    })} />)
    expect(screen.getByTestId('update-outcome').getAttribute('data-state')).toBe('rolled_back')
    expect(screen.getByText('The update to 0.9.0 was rolled back')).toBeTruthy()
    expect(screen.getByText(/Still running 0.8.2/)).toBeTruthy()
  })

  it('tells everyone the restart is an update, not an outage', () => {
    renderWithProviders(<ControlUpdatingBanner target="0.9.0" unreachable />)
    expect(screen.getByTestId('control-updating-banner').textContent).toContain('restarting on 0.9.0')
    expect(screen.getByTestId('control-updating-banner').textContent).toContain('sample data')
  })

  it('checks for a newer release on demand, and shows what it found', async () => {
    live.checkControlUpdate.mockResolvedValueOnce(view({
      running: '0.9.0',
      newest: { version: '0.9.1', publishedAt: '2026-11-09T09:00:00Z', page: '' },
      checkedAt: new Date().toISOString(),
    }))
    renderWithProviders(<ControlUpdatePanel v={view({ running: '0.9.0', newer: false, rollback: undefined })} />)
    fireEvent.click(screen.getByTestId('update-check'))
    await waitFor(() => expect(live.checkControlUpdate).toHaveBeenCalledTimes(1))
    await waitFor(() => expect(notify.notifyOk).toHaveBeenCalledWith('argus-control 0.9.1 is out', 'This control plane runs 0.9.0.'))
  })

  it('says why a check failed', async () => {
    live.checkControlUpdate.mockResolvedValueOnce(view({ newer: false, checkFailure: 'api.github.com answered 403 Forbidden' }))
    renderWithProviders(<ControlUpdatePanel v={view({ newer: false, rollback: undefined })} />)
    fireEvent.click(screen.getByTestId('update-check'))
    await waitFor(() => expect(notify.notifyWarn).toHaveBeenCalledWith(
      'Could not check for releases', 'api.github.com answered 403 Forbidden'))
  })

  it('offers no check when this control plane does not look for releases', () => {
    renderWithProviders(<ControlUpdatePanel v={view({ releaseChecks: false, checkedAt: undefined })} />)
    expect(screen.queryByTestId('update-check')).toBeNull()
    expect(screen.getByText('Release checks are off.')).toBeTruthy()
  })

  it('does not check while an update runs', () => {
    const at = new Date().toISOString()
    renderWithProviders(<ControlUpdatePanel v={view({
      update: { id: 'u1', from: '0.8.2', target: '0.9.0', state: 'installing', message: 'installing', at, steps: [] },
    })} />)
    expect((screen.getByTestId('update-check') as HTMLButtonElement).disabled).toBe(true)
  })
})
