import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

/**
 * Telling "the server refused" apart from "there is no server".
 *
 * Collapsing the two is what made signing in impossible: /auth/me answers 401
 * for a browser that has not signed in, whoami reported that as unreachable,
 * and LoginGate checks `unreachable` before `oidcEnabled` -- so the console
 * showed an outage message and never rendered the sign-in button at all.
 *
 * The same conflation in get() was worse: one refused request marked the whole
 * deployment unreachable, and orFallback then answered with the demo fixture.
 * A real deployment would show invented hosts and sessions, labelled as real.
 *
 * These tests load live.ts with a control plane configured, which the rest of
 * the suite deliberately does not -- and which is precisely why none of this
 * was covered.
 */

// isConfigured() reads a build-time constant, so the module has to be loaded
// fresh with the env set rather than stubbed after the fact.
async function loadLive() {
  vi.resetModules()
  vi.stubEnv('VITE_CONTROL_URL', '/')
  return import('~/lib/live')
}

const json = (status: number, body: unknown) =>
  new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })

beforeEach(() => {
  vi.restoreAllMocks()
})
afterEach(() => {
  vi.unstubAllEnvs()
  vi.unstubAllGlobals()
})

describe('whoami', () => {
  it('reports a 401 as "not signed in", carrying what the sign-in screen needs', async () => {
    const { whoami } = await loadLive()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      json(401, { authenticated: false, loginUrl: '/auth/login', oidcEnabled: true }),
    ))

    const id = await whoami()
    // The button is only rendered when this is falsy. This single field is
    // what stood between an operator and the ability to log in at all.
    expect(id.unreachable).toBeFalsy()
    expect(id.authenticated).toBe(false)
    expect(id.oidcEnabled).toBe(true)
    expect(id.loginUrl).toBe('/auth/login')
  })

  it('reports a signed-in session', async () => {
    const { whoami } = await loadLive()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      json(200, { authenticated: true, email: 'lin@northwind.id', role: 'admin' }),
    ))
    const id = await whoami()
    expect(id).toMatchObject({ authenticated: true, role: 'admin' })
    expect(id.unreachable).toBeFalsy()
  })

  it('reports a transport failure as unreachable', async () => {
    const { whoami } = await loadLive()
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    expect(await whoami()).toMatchObject({ authenticated: false, unreachable: true })
  })

  // A proxy that cannot reach the control plane is genuinely an outage, and
  // an operator sent to check their identity provider instead would waste an
  // afternoon on the wrong thing.
  it('reports a gateway error as unreachable', async () => {
    const { whoami } = await loadLive()
    for (const status of [502, 503, 504]) {
      vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(status, {})))
      expect((await whoami()).unreachable, `status ${status}`).toBe(true)
    }
  })
})

describe('isLive', () => {
  it('stays true when the control plane refuses a request', async () => {
    const { live, isLive } = await loadLive()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(403, { error: 'forbidden' })))

    await expect(live.assets()).rejects.toThrow()
    // It answered. Refusing one request is not being absent, and treating it
    // as absence is what made the console fall back to demo data.
    expect(isLive()).toBe(true)
  })

  it('goes false only when nothing answers', async () => {
    const { live, isLive } = await loadLive()
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))
    await expect(live.assets()).rejects.toThrow()
    expect(isLive()).toBe(false)
  })
})

describe('ControlPlaneError', () => {
  it('carries the status and marks an authorization refusal', async () => {
    const { live, ControlPlaneError } = await loadLive()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(403, {})))

    await expect(live.assets()).rejects.toBeInstanceOf(ControlPlaneError)

    let caught: unknown
    try {
      await live.assets()
    } catch (e) {
      caught = e
    }
    if (!(caught instanceof ControlPlaneError)) {
      throw new Error(`expected a ControlPlaneError, got ${String(caught)}`)
    }
    expect(caught.status).toBe(403)
    expect(caught.refused).toBe(true)
    expect(caught.path).toContain('/api/v1/assets')
  })
})

describe('falling back to the fixture', () => {
  /**
   * The rule this protects: the console must never present fixture data as
   * real. A refused request answered with invented hosts and sessions is
   * exactly that, and it is worse than an error because nothing on screen
   * says anything is wrong.
   */
  it('does not answer a refusal with demo data', async () => {
    const { live } = await loadLive()
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(json(403, { error: 'forbidden' })))

    const fixture = vi.fn().mockResolvedValue(['a fabricated asset'])
    await expect(live.orFallback(live.assets, fixture)).rejects.toThrow()
    expect(fixture, 'the fixture must not be consulted for a refusal').not.toHaveBeenCalled()
  })

  it('falls back when the control plane is genuinely absent', async () => {
    const { live } = await loadLive()
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new TypeError('Failed to fetch')))

    const fixture = vi.fn().mockResolvedValue(['fixture'])
    await expect(live.orFallback(live.assets, fixture)).resolves.toEqual(['fixture'])
    expect(fixture).toHaveBeenCalled()
  })

  it('uses the fixture when no control plane is configured at all', async () => {
    vi.resetModules()
    vi.stubEnv('VITE_CONTROL_URL', '')
    const { live } = await import('~/lib/live')
    const fixture = vi.fn().mockResolvedValue(['fixture'])
    await expect(live.orFallback(live.assets, fixture)).resolves.toEqual(['fixture'])
  })
})
