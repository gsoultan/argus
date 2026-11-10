import { describe, expect, it } from 'vitest'
import { protocolOf, waysFor } from '~/lib/sessions'
import type { Asset } from '~/types/domain'

/** What the connect panel offers for each kind of asset. */

const asset = (a: Partial<Asset>) => a as Asset

describe('waysFor', () => {
  it('offers a Windows server its desktop, a terminal and its files', () => {
    expect(waysFor(asset({ protocol: 'rdp' }))).toEqual(['desktop', 'terminal', 'files', 'split'])
    expect(waysFor(asset({ protocol: 'rdp', desktopMode: 'screen' }))).toEqual(['screen', 'terminal', 'files', 'split'])
  })

  it('offers a Linux server a terminal and files', () => {
    expect(waysFor(asset({ protocol: 'ssh' }))).toEqual(['terminal', 'files', 'split'])
  })

  it('opens a Windows terminal as a terminal, not a desktop', () => {
    expect(protocolOf('terminal')).toBe('ssh')
    expect(protocolOf('files')).toBe('ssh')
    expect(protocolOf('desktop')).toBe('rdp')
  })
})
