import { describe, expect, it } from 'vitest'
import { warningBadge, warningText, warningTone } from '~/lib/resourcesLow'
import type { ResourceWarning } from '~/types/domain'

/** How a warning from the control plane is written on the card, the list and the header. */

const GB = 1024 ** 3
const mem: ResourceWarning = { resource: 'memory', level: 'critical', percent: 4, left: 1.4 * GB, total: 32 * GB }
const disk: ResourceWarning = { resource: 'disk', level: 'low', mount: 'C:', percent: 92, left: 39 * GB, total: 476 * GB }
const cpu: ResourceWarning = { resource: 'cpu', level: 'low', percent: 93, left: 0, total: 0 }
const swap: ResourceWarning = { resource: 'swap', level: 'low', percent: 94, left: 0.25 * GB, total: 4 * GB }

describe('resource warnings', () => {
  it('says what and how much, in a badge and a line', () => {
    expect([mem, disk, cpu, swap].map(warningBadge)).toEqual(['Memory 4% left', 'Disk C: 92% full', 'CPU 93%', 'Swap 94% used'])
    expect(warningText(mem)).toBe('Memory: 1.4 GB of 32 GB left (4%)')
    expect(warningText(disk)).toBe('Disk C: is 92% full, 39 GB left')
    expect(warningText(swap)).toBe('Swap: 94% used, 256 MB left')
  })

  it('is rose when critical, amber when low, and nothing without a warning', () => {
    expect(warningTone(mem)).toBe('rose')
    expect(warningTone(disk)).toBe('amber')
    expect(warningTone(undefined)).toBeUndefined()
  })
})
