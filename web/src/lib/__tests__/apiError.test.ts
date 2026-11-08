import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  NetworkError, describeStatus, errorText, humanizeError, noteResponse, onSessionExpired,
} from '~/lib/apiError'

/**
 * What the console says when a request fails.
 *
 * It used to say "/api/v1/databases → 401": a path and a number, shown to
 * someone who wanted to know whether to sign in, ask for access, or wait.
 */
describe('humanizeError', () => {
  it('describes a status in words when the server said nothing useful', () => {
    expect(humanizeError(401)).toBe('Your session has ended. Sign in again to continue.')
    expect(humanizeError(401, 'unauthorized')).toBe(describeStatus(401))
    expect(humanizeError(403, 'Forbidden.')).toMatch(/do not have permission/)
    expect(humanizeError(404, '')).toMatch(/could not be found/)
    expect(humanizeError(503)).toMatch(/not available right now/)
    expect(humanizeError(500)).toMatch(/went wrong on the server/)
  })

  it('keeps the control plane\'s own explanation, as a sentence', () => {
    expect(humanizeError(401, 'your current password is not correct'))
      .toBe('Your current password is not correct.')
    expect(humanizeError(403, 'You need the auditor role.')).toBe('You need the auditor role.')
  })

  it('never shows a bare path and status', () => {
    for (const status of [400, 401, 403, 404, 409, 413, 429, 500, 502, 503, 504, 418]) {
      expect(humanizeError(status)).not.toMatch(/→|\/api\//)
    }
  })
})

describe('errorText', () => {
  it('replaces the browser\'s wording for a request that got no answer', () => {
    expect(errorText(new TypeError('Failed to fetch'))).toMatch(/could not be reached/)
    expect(errorText(new TypeError('Load failed'))).toMatch(/could not be reached/)
    expect(errorText(new NetworkError())).toMatch(/could not be reached/)
  })

  it('passes other messages through', () => {
    expect(errorText(new Error('Name is required.'))).toBe('Name is required.')
    expect(errorText(undefined, 'fallback')).toBe('fallback')
  })
})

describe('noteResponse', () => {
  const stop: Array<() => void> = []
  afterEach(() => { while (stop.length) stop.pop()!() })

  it('announces a 401 from the API', () => {
    const heard = vi.fn()
    stop.push(onSessionExpired(heard))
    noteResponse('/api/v1/databases', 401)
    expect(heard).toHaveBeenCalledTimes(1)
  })

  it('ignores other statuses, and a 401 that is an answer from /auth', () => {
    const heard = vi.fn()
    stop.push(onSessionExpired(heard))
    noteResponse('/api/v1/databases', 403)
    noteResponse('/api/v1/databases', 200)
    noteResponse('/auth/me', 401)
    noteResponse('https://argus.example/auth/password', 401)
    expect(heard).not.toHaveBeenCalled()
  })
})
