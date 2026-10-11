import { describe, expect, it } from 'vitest'
import { describeTunnel } from '~/components/TunnelStatus'

describe('describeTunnel', () => {
  it('never calls a tunnel connected without the control plane saying so', () => {
    expect(describeTunnel(undefined).label).toBe('Not connected')
    expect(describeTunnel({ connected: false }).label).toBe('Not connected')
  })

  it('says when the agent is on the slower HTTPS fallback', () => {
    const d = describeTunnel({ connected: true, transport: 'tcp', rttMillis: 41.6 })
    expect(d.label).toBe('Connected over HTTPS · 42 ms')
    expect(d.detail).toMatch(/UDP/)
  })

  it('shows the measured round trip on QUIC', () => {
    expect(describeTunnel({ connected: true, transport: 'quic', rttMillis: 3.21 }).label)
      .toBe('Connected · 3.2 ms')
  })
})
