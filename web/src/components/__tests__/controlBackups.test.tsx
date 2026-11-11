import { fireEvent, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ControlBackupsPanel } from '~/components/ControlBackups'
import type { ControlBackupsView } from '~/lib/controlBackups'
import { renderWithProviders } from '~/test/render'

const live = vi.hoisted(() => ({ send: vi.fn() }))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  ...live,
}))

/**
 * Settings › Control plane › Backups: the last run, a backup now, how many to
 * keep, and what is on disk -- all from what the control plane read.
 */

const run = (name: string, complete = true) => ({
  name, complete, at: '2026-11-0' + name.slice(7, 8) + 'T02:00:00Z', databaseBytes: 5 * 1024 * 1024,
  recordings: 120, chainEvents: 4000, version: '1.2.22',
})

const view = (over: Partial<ControlBackupsView> = {}): ControlBackupsView => ({
  installed: true,
  canRunNow: true,
  timerOn: true,
  dir: '/var/backups/argus',
  keep: 2,
  keepSetHere: false,
  last: { startedAt: '2026-11-10T02:00:00Z', finishedAt: '2026-11-10T02:01:00Z', ok: true },
  running: false,
  runs: [run('20261103T020000Z'), run('20261102T020000Z', false), run('20261101T020000Z'), run('20261100T020000Z')],
  ...over,
})

describe('The control plane\'s own backups', () => {
  it('says how the last one went and which ones the next removes', () => {
    renderWithProviders(<ControlBackupsPanel v={view()} />)
    expect(screen.getByTestId('backup-state').getAttribute('data-tone')).toBe('ok')
    expect(screen.getByText('Did not finish')).toBeTruthy()
    // Keep 2: the third complete one goes next.
    expect(screen.getAllByText('Removed by the next backup')).toHaveLength(1)
    expect((screen.getByTestId('backup-now') as HTMLButtonElement).disabled).toBe(false)
  })

  it('shows the error of a failed backup', () => {
    renderWithProviders(<ControlBackupsPanel v={view({
      last: { startedAt: '2026-11-10T02:00:00Z', finishedAt: '2026-11-10T02:00:01Z', ok: false, error: 'pg_dump is not installed' },
    })} />)
    expect(screen.getByTestId('backup-state').getAttribute('data-tone')).toBe('bad')
    expect(screen.getByText('pg_dump is not installed')).toBeTruthy()
  })

  it('does not offer a second backup while one runs', () => {
    renderWithProviders(<ControlBackupsPanel v={view({ running: true })} />)
    expect(screen.getByText('Backing up now')).toBeTruthy()
    expect((screen.getByTestId('backup-now') as HTMLButtonElement).disabled).toBe(true)
  })

  it('says how to turn on the nightly backup when it is off', () => {
    renderWithProviders(<ControlBackupsPanel v={view({ timerOn: false })} />)
    expect(screen.getByText('Nightly backup off')).toBeTruthy()
    expect(screen.getByText(/systemctl enable --now argus-backup.timer/)).toBeTruthy()
  })

  it('saves how many to keep', async () => {
    live.send.mockResolvedValue(view({ keep: 5, keepSetHere: true }))
    renderWithProviders(<ControlBackupsPanel v={view()} />)
    const save = screen.getByTestId('backup-keep-save') as HTMLButtonElement
    expect(save.disabled).toBe(true)
    fireEvent.change(screen.getByTestId('backup-keep'), { target: { value: '5' } })
    await waitFor(() => expect(save.disabled).toBe(false))
    fireEvent.click(save)
    await waitFor(() => expect(live.send).toHaveBeenCalledWith('PUT', '/api/v1/platform/backups/settings', { keep: 5 }))
  })
})
