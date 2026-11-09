import { describe, expect, it } from 'vitest'
import { removalWords } from '~/components/server/SettingsTab'
import type { Asset } from '~/types/domain'

/**
 * Removing an asset says what its agent will then do, which depends on what
 * it is: a device's app is not in sshd, and a Windows host has no Argus-made
 * accounts to lock.
 */
const base = { id: 'a1', hostname: 'h1' } as unknown as Asset

describe('removalWords', () => {
  it('tells a device what the Argus app does, not what sshd does', () => {
    const w = removalWords({ ...base, kind: 'android' } as Asset, true)
    expect(w.then).toContain('revokes its own link')
    expect(w.then).toContain('accessibility service')
    expect(w.then).not.toMatch(/SSH/)
    const later = removalWords({ ...base, kind: 'android' } as Asset, false)
    expect(later.then).toContain('Remove from Argus')
  })

  it('tells a Linux server its accounts are locked and SSH works again', () => {
    const w = removalWords({ ...base, kind: 'server' } as Asset, true)
    expect(w.then).toContain('out of SSH')
    expect(w.then).toContain('locks the accounts Argus made')
    expect(removalWords({ ...base, kind: 'server' } as Asset, false).then).toContain('sudo argus-agent uninstall')
  })

  it('tells a Windows host what is left in Apps, not about SSH', () => {
    const w = removalWords({ ...base, kind: 'workstation', protocol: 'rdp' } as Asset, true)
    expect(w.then).toContain('workstation')
    expect(w.then).toContain('Apps')
    expect(w.then).not.toMatch(/SSH/)
  })
})
