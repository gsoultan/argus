import { describe, expect, it } from 'vitest'
import { rollUp, type CheckGroup, type WebsiteCheck } from '~/lib/websites'
import { sectionsOf } from '~/components/Websites'
import { groupsFor } from '~/components/nav'

const site = (name: string, state: WebsiteCheck['state'], groupId = '', paused = false) =>
  ({ id: name, name, state, groupId, paused }) as WebsiteCheck

describe('Monitoring groups', () => {
  it('rolls a group up to its worst check, leaving paused ones out', () => {
    expect(rollUp([])).toBe('empty')
    expect(rollUp([site('a', 'up'), site('b', 'down')])).toBe('down')
    expect(rollUp([site('a', 'up'), site('b', 'unknown')])).toBe('unknown')
    expect(rollUp([site('a', 'up'), site('b', 'down', '', true)])).toBe('up')
    expect(rollUp([site('a', 'down', '', true)])).toBe('paused')
  })

  it('puts the group with a site down first, and its down site first in it', () => {
    const groups: CheckGroup[] = [{ id: 'g1', name: 'Apps', checks: 2 }, { id: 'g2', name: 'Shops', checks: 2 }, { id: 'g3', name: 'Empty', checks: 0 }]
    const list = [site('app', 'up', 'g1'), site('shop', 'up', 'g2'), site('pay', 'down', 'g2'), site('blog', 'up'), site('lost', 'up', 'gone')]
    const secs = sectionsOf(list, groups, false)
    expect(secs.map((s) => s.group?.name ?? 'none')).toEqual(['Shops', 'Apps', 'none'])
    expect(secs[0]!.checks.map((c) => c.name)).toEqual(['pay', 'shop'])
    // A check whose group is not listed (removed meanwhile) is shown ungrouped, not lost.
    expect(secs[2]!.checks.map((c) => c.name)).toEqual(['blog', 'lost'])
    // Admins also see an empty group, to fill it.
    expect(sectionsOf(list, groups, true).map((s) => s.group?.name ?? 'none')).toContain('Empty')
  })

  it('shows Websites in the menu to someone given a check, and not otherwise', () => {
    const has = (seesChecks: boolean) =>
      groupsFor('operator', seesChecks).flatMap((g) => g.items.map((i) => i.to)).includes('/websites')
    expect(has(false)).toBe(false)
    expect(has(true)).toBe(true)
    expect(groupsFor('auditor').flatMap((g) => g.items.map((i) => i.to))).toContain('/websites')
  })
})
