import { fireEvent, screen, waitFor } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { FleetRecoveryKey } from '~/components/FleetRecoveryKey'
import { readinessChecks, type ReadinessView } from '~/lib/controlReadiness'
import { holdingLabel } from '~/lib/recoveryKey'
import { renderWithProviders } from '~/test/render'

const live = vi.hoisted(() => ({ get: vi.fn(), send: vi.fn(), isConfigured: () => true }))
vi.mock('~/lib/live', async (orig) => ({ ...(await orig<typeof import('~/lib/live')>()), ...live }))

/** Settings › Account security › Fleet recovery key. */

describe('the fleet recovery key', () => {
  it('counts the servers holding it', () => {
    expect(holdingLabel({ holding: 17, total: 18 })).toBe('17 of 18')
    expect(holdingLabel({ holding: 0, total: 0 })).toMatch(/No servers/)
  })

  it('shows the private half once and asks that it was kept before closing', async () => {
    URL.createObjectURL = vi.fn(() => 'blob:x')
    URL.revokeObjectURL = vi.fn()
    live.get.mockResolvedValue({ set: false, holding: 0, total: 3 })
    live.send.mockResolvedValue({ fingerprint: 'ab12-cd34-ef56-7890', privateKey: 'ARGUS FLEET RECOVERY KEY v1\nSEED', fileName: 'argus-recovery-ab12.key' })
    renderWithProviders(<FleetRecoveryKey />)
    fireEvent.click(await screen.findByRole('button', { name: 'Make the recovery key' }))
    await waitFor(() => expect(live.send).toHaveBeenCalledWith('POST', '/api/v1/platform/recovery-key'))
    expect(await screen.findByTestId('recovery-private')).toHaveTextContent('SEED')
    const done = screen.getByRole('button', { name: 'Done' })
    expect(done).toBeDisabled()
    fireEvent.click(screen.getByLabelText(/kept it somewhere/))
    expect(done).toBeEnabled()
  })
})

describe('the readiness page', () => {
  const v = (recovery: ReadinessView['recovery']): ReadinessView => ({
    outsideCheck: { configured: true }, servers: [], running: 1,
    backups: { installed: true, timerOn: true, lastOk: true }, directSsh: { refusing: 0, total: 0 },
    confirmations: { backups_off_host: null, key_off_host: null, provider_consoles: null }, confirmDays: 90, recovery,
  })
  const line = (x: ReadinessView) => readinessChecks(x, Date.now()).find((l) => l.id === 'recovery')
  it('asks for one when there is none, and counts servers without it', () => {
    expect(line(v({ set: false, holding: 0, total: 4 }))?.tone).toBe('warn')
    expect(line(v({ set: true, holding: 3, total: 4 }))?.title).toMatch(/^1 of 4/)
    expect(line(v({ set: true, holding: 4, total: 4 }))?.tone).toBe('ok')
    expect(line(v(undefined))).toBeUndefined()
  })
})
