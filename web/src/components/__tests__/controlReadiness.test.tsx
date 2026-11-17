import { fireEvent, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { ControlReadinessPanel, readinessNeeds } from '~/components/ControlReadiness'
import { outsideUrlError } from '~/components/ControlSettings'
import { readinessChecks, type ReadinessView } from '~/lib/controlReadiness'
import { renderWithProviders } from '~/test/render'

const live = vi.hoisted(() => ({ send: vi.fn() }))

vi.mock('~/lib/live', async (orig) => ({
  ...(await orig<typeof import('~/lib/live')>()),
  ...live,
}))

/**
 * Settings › Control plane › If Argus goes down: what Argus can see about its
 * own outages, and what only a person can confirm.
 */

const NOW = Date.parse('2026-11-17T03:00:00Z')
const ago = (ms: number) => new Date(NOW - ms).toISOString()

const server = { host: 'ctl-01', version: '1.3.0', startedAt: ago(3600_000), seenAt: ago(20_000), running: true }

const view = (over: Partial<ReadinessView> = {}): ReadinessView => ({
  outsideCheck: { configured: true, host: 'hc-ping.com', every: '1m', lastAt: ago(30_000), ok: true, detail: '200 OK' },
  servers: [server],
  running: 1,
  backups: { installed: true, timerOn: true, lastAt: ago(5 * 3600_000), lastOk: true },
  directSsh: { refusing: 14, total: 18 },
  confirmations: { backups_off_host: null, key_off_host: null, provider_consoles: null },
  confirmDays: 90,
  ...over,
})

const tone = (v: ReadinessView, id: string) => readinessChecks(v, NOW).find((l) => l.id === id)?.tone

describe('what Argus checks for itself', () => {
  it('says nothing outside will notice when no check-in is set', () => {
    expect(tone(view({ outsideCheck: { configured: false } }), 'outside')).toBe('bad')
  })
  it('warns about a check-in that is set but has not run in ten minutes', () => {
    expect(tone(view({ outsideCheck: { configured: true, host: 'x', lastAt: ago(11 * 60_000), ok: true } }), 'outside')).toBe('warn')
  })
  it('calls a failed check-in bad, and a recent good one ok', () => {
    expect(tone(view({ outsideCheck: { configured: true, lastAt: ago(1000), ok: false } }), 'outside')).toBe('bad')
    expect(tone(view(), 'outside')).toBe('ok')
  })
  it('warns when one control server runs alone, not when two do', () => {
    expect(tone(view(), 'servers')).toBe('warn')
    const two = view({ servers: [...view().servers, { ...server, host: 'ctl-02' }], running: 2 })
    expect(tone(two, 'servers')).toBe('ok')
  })
  it('a server last heard from long ago does not count as running', () => {
    const stale = view({ servers: [...view().servers, { ...server, host: 'old', running: false }] })
    expect(tone(stale, 'servers')).toBe('warn')
  })
  it('flags a failed, missing or day-old backup', () => {
    expect(tone(view({ backups: { installed: false, timerOn: false, lastOk: false } }), 'backups')).toBe('bad')
    expect(tone(view({ backups: { installed: true, timerOn: true, lastAt: ago(1000), lastOk: false, lastError: 'pg_dump' } }), 'backups')).toBe('bad')
    expect(tone(view({ backups: { installed: true, timerOn: true, lastAt: ago(40 * 3600_000), lastOk: true } }), 'backups')).toBe('warn')
    expect(tone(view(), 'backups')).toBe('ok')
  })
  it('counts the servers that refuse plain SSH while Argus is down', () => {
    const l = readinessChecks(view(), NOW).find((x) => x.id === 'ssh')
    expect(l?.tone).toBe('warn')
    expect(l?.title).toBe('14 of 18 servers refuse plain SSH while Argus is down')
  })
})

describe('the page', () => {
  it('scores what is ready, what needs someone and what is to confirm', () => {
    renderWithProviders(<ControlReadinessPanel v={view({
      confirmations: {
        backups_off_host: { by: 'g@x', at: ago(1000), until: ago(-1000), lapsed: false },
        key_off_host: { by: 'g@x', at: ago(100 * 86400_000), until: ago(10 * 86400_000), lapsed: true },
        provider_consoles: null,
      },
    })} now={NOW} />)
    // ok: outside, backups, backups_off_host. warn: servers, ssh, lapsed key. ask: consoles.
    expect(screen.getByTestId('readiness-score')).toHaveTextContent('3 of 7 ready · 3 need you · 1 to confirm')
    expect(screen.getByTestId('confirm-key_off_host')).toHaveAttribute('data-tone', 'warn')
    expect(screen.getByTestId('confirm-key_off_host')).toHaveTextContent('check it again')
    expect(screen.getByTestId('confirm-provider_consoles')).toHaveAttribute('data-tone', 'ask')
    expect(screen.getByTestId('runbook-link')).toHaveAttribute('href', expect.stringContaining('docs/argus-down.md'))
  })

  it('confirms through the control plane', async () => {
    live.send.mockResolvedValue({ by: 'g@x', at: ago(0), until: ago(-90 * 86400_000), lapsed: false })
    renderWithProviders(<ControlReadinessPanel v={view()} now={NOW} />)
    fireEvent.click(screen.getByTestId('confirm-provider_consoles-button'))
    await waitFor(() => expect(live.send).toHaveBeenCalledWith('POST', '/api/v1/platform/readiness/provider_consoles/confirm'))
  })

  it('counts what needs a person for the menu', () => {
    // servers + ssh warn, three unconfirmed.
    expect(readinessNeeds(view(), NOW)).toBe(5)
  })
})

describe('the check-in address', () => {
  it('takes http and https addresses with a host and no credentials', () => {
    expect(outsideUrlError('https://hc-ping.com/abc')).toBeUndefined()
    expect(outsideUrlError('http://kuma.lan:3001/api/push/x')).toBeUndefined()
    expect(outsideUrlError('ftp://x/y')).toBe('Use an https:// address')
    expect(outsideUrlError('https://u:p@hc-ping.com/x')).toBe('Put no user name or password in the address')
    expect(outsideUrlError('nonsense')).toBe('Not an address')
  })
})
