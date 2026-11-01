import { afterEach, describe, expect, it } from 'vitest'
import { argusVersion } from '~/lib/live'

/**
 * The version on About and in the account menu is the one the control plane
 * was stamped with from its release tag, read from /console-config.js.
 */
const g = globalThis as { __ARGUS__?: { version?: string } }

describe('argusVersion', () => {
  afterEach(() => {
    delete g.__ARGUS__
  })

  it('shows a release tag with its v', () => {
    g.__ARGUS__ = { version: '0.6.0' }
    expect(argusVersion()).toBe('v0.6.0')
  })

  it('keeps a tag that already has its v, and a build between tags', () => {
    g.__ARGUS__ = { version: 'v0.6.0-3-gabc1234' }
    expect(argusVersion()).toBe('v0.6.0-3-gabc1234')
  })

  it('says dev for an untagged build or no control plane', () => {
    g.__ARGUS__ = { version: 'dev' }
    expect(argusVersion()).toBe('dev')
    delete g.__ARGUS__
    expect(argusVersion()).toBe('dev')
  })
})
