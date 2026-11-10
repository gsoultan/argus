import { afterEach, describe, expect, it } from 'vitest'
import { installOneLiner, type EnrollToken } from '~/lib/live'

// The page loaded with the old "Address servers use"; the control plane has
// since been saved with a new one. The command must use what it says now.
describe('installOneLiner', () => {
  const g = globalThis as { __ARGUS__?: { fleetUrl?: string } }
  afterEach(() => { delete g.__ARGUS__ })

  const token: EnrollToken = { id: '1', token: 'argus-enrol-abc', expiresAt: '2026-01-01T00:00:00Z' }

  it('uses the address the control plane returned over the one the page loaded', () => {
    g.__ARGUS__ = { fleetUrl: 'https://argus.example.com' }
    const cmd = installOneLiner({ ...token, fleetUrl: 'https://argus-internal.example.com:8080/' }, '1.0.0')
    expect(cmd).toContain("'https://argus-internal.example.com:8080/api/v1/install/agent.sh?")
    expect(cmd).toContain('url=https%3A%2F%2Fargus-internal.example.com%3A8080')
    expect(cmd).not.toContain('//argus.example.com')
  })

  it('falls back to the page setting when the control plane returns none', () => {
    g.__ARGUS__ = { fleetUrl: 'https://argus.example.com' }
    expect(installOneLiner(token, '1.0.0')).toContain("'https://argus.example.com/api/v1/install/agent.sh?")
  })

  // Behind a Cloudflare Tunnel with split DNS the control plane sees itself at
  // its own address and hands out its pin; a server outside that network meets
  // Cloudflare there, and the pinned fetch alone failed with "public key does
  // not match pinned public key".
  it('falls back to the verified fetch when the pinned one fails', () => {
    const pin = 'sha256//q83vEjRWeJA='
    const cmd = installOneLiner({ ...token, fleetUrl: 'https://argus.example.com', curlPin: pin }, '1.0.0')
    const url = "'https://argus.example.com/api/v1/install/agent.sh?"
    expect(cmd.startsWith(`{ curl -fsSL -k --pinnedpubkey '${pin}' ${url}`)).toBe(true)
    expect(cmd).toContain(`2>/dev/null || curl -fsSL ${url}`)
    expect(cmd.endsWith('; } | sudo sh')).toBe(true)
    // The script still gets the pin; the agent falls back the same way.
    expect(cmd).not.toContain('pin=off')
  })

  it('has no pin and no fallback behind a public certificate', () => {
    const cmd = installOneLiner({ ...token, fleetUrl: 'https://argus.example.com' }, '1.0.0')
    expect(cmd).toMatch(/^curl -fsSL 'https:\/\/argus\.example\.com\/api\/v1\/install\/agent\.sh\?[^']*pin=off' \| sudo sh$/)
  })
})
