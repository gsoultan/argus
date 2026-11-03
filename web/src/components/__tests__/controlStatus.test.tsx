import { fireEvent, screen } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { StatusView } from '~/components/ControlStatus'
import type { ControlStatus } from '~/lib/live'
import { renderWithProviders } from '~/test/render'

/**
 * The Status page shows what argus-control reported and nothing it did not:
 * the pill, the tiles and the checks all follow the problems the control
 * plane listed, so they cannot disagree with each other.
 */

const GB = 1024 ** 3

const healthy = (over: Partial<ControlStatus> = {}): ControlStatus => ({
  version: 'v0.9.2',
  hostname: 'argus-01',
  startedAt: '2026-10-22T09:14:00Z',
  uptimeSeconds: 12 * 86400,
  checkedAt: '2026-11-03T09:14:00Z',
  machine: {
    os: 'Ubuntu 24.04.1 LTS', cores: 4, load1: 0.48, cpuBusy: 12, memTotal: 8 * GB, memAvailable: 5 * GB,
    disks: [{ path: '/tmp', use: 'Recordings arriving from agents', total: 100 * GB, avail: 62 * GB }],
  },
  process: { memoryBytes: 214 * 1024 ** 2, goroutines: 1846, tunnels: 41, goVersion: 'go1.27.1' },
  database: {
    ok: true, latencyMs: 4, version: '17.4', name: 'argus', inUse: 7, open: 9, max: 20,
    sizeBytes: 6.8 * GB, tables: [{ name: 'audit_events', bytes: 3.9 * GB }, { name: '', bytes: 2.9 * GB }],
    measuredAt: '2026-11-03T09:11:00Z',
  },
  fleet: {
    organizations: 1, agentsEnrolled: 44, agentsConnected: 41, sessionsActive: 6, sessionsSilent: 0,
    sessionsToday: 212, measuredAt: '2026-11-03T09:11:00Z',
  },
  recordings: {
    where: 'bucket', location: 'argus-recordings', count: 3212, bytes: 182 * GB, weekBytes: 9.4 * GB,
    oldest: '2025-11-04T00:00:00Z', measuredAt: '2026-11-03T09:11:00Z', lastUpload: '2026-11-03T09:12:00Z',
  },
  certificate: { subject: 'argus.impack.co.id', notAfter: '2027-03-14T00:00:00Z', daysLeft: 131, managed: false },
  errors: { lastHour: 0, sinceStart: 3, recent: [] },
  problems: [],
  ...over,
})

describe('Control plane status', () => {
  it('says healthy only when the control plane listed no problems', () => {
    renderWithProviders(<StatusView st={healthy()} onOpen={vi.fn()} />)
    expect(screen.getByTestId('status-pill').textContent).toBe('Healthy')
    expect(screen.getByText('Audit log')).toBeTruthy()
    expect(screen.getByText('Everything else')).toBeTruthy()
    expect(screen.getByTestId('check-Database').getAttribute('data-state')).toBe('ok')
    expect(screen.getByTestId('check-Certificate').getAttribute('data-state')).toBe('ok')
    expect(screen.getByText('None.')).toBeTruthy()
  })

  it('counts the problems, puts critical ones on top, and links each to its setting', () => {
    const onOpen = vi.fn()
    const st = healthy({
      database: { ...healthy().database, latencyMs: 310, inUse: 19 },
      machine: { ...healthy().machine, disks: [{ path: '/tmp', use: 'spool', total: 100 * GB, avail: 6 * GB }] },
      certificate: { subject: 'argus.impack.co.id', notAfter: '2026-11-17T00:00:00Z', daysLeft: 14, managed: false },
      errors: {
        lastHour: 27, sinceStart: 41,
        recent: [{ at: '2026-11-03T09:10:58Z', op: 'sessions', message: 'timeout acquiring a connection', count: 14 }],
      },
      problems: [
        { severity: 'critical', area: 'disk', title: 'The disk at /tmp is 94% full', detail: 'Recordings cannot be stored.' },
        { severity: 'warning', area: 'database', title: 'The database took 310 ms to answer' },
        { severity: 'warning', area: 'certificate', title: 'The certificate expires in 14 days' },
      ],
    })
    renderWithProviders(<StatusView st={st} onOpen={onOpen} />)

    expect(screen.getByTestId('status-pill').textContent).toBe('3 problems')
    expect(screen.getByText('The disk at /tmp is 94% full')).toBeTruthy()
    expect(screen.getByTestId('status-disk-/tmp').getAttribute('data-tone')).toBe('rose')
    expect(screen.getByTestId('check-Database').getAttribute('data-state')).toBe('warn')
    expect(screen.getByTestId('check-Certificate').getAttribute('data-state')).toBe('warn')
    expect(screen.getByText('×14')).toBeTruthy()

    fireEvent.click(screen.getByText('Database settings ›'))
    expect(onOpen).toHaveBeenCalledWith('database')
    fireEvent.click(screen.getByText('Certificate ›'))
    expect(onOpen).toHaveBeenCalledWith('certificate')
  })

  it('does not invent fleet numbers when the database could not be asked', () => {
    const st = healthy({
      database: { ok: false, error: 'connection refused', latencyMs: 3000, inUse: 0, open: 0, max: 20, tables: [] },
      fleet: undefined,
      problems: [{ severity: 'critical', area: 'database', title: 'The database is not answering' }],
    })
    renderWithProviders(<StatusView st={st} onOpen={vi.fn()} />)
    expect(screen.getByTestId('check-Database').getAttribute('data-state')).toBe('bad')
    expect(screen.getAllByText('Needs the database').length).toBe(2)
    expect(screen.getByText('Down')).toBeTruthy()
  })
})
