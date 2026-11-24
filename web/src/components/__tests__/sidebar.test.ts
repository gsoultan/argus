import { describe, expect, it } from 'vitest'
import { bottomFor, groupsFor, isItemActive } from '~/components/nav'
import { cleanPrefs } from '~/lib/preferences'

/*
 * The sidebar: the same pages as before, in six groups, each with an icon so
 * it still reads folded; and the entry for the page on screen, and only it,
 * lit.
 */

describe('the sidebar', () => {
  it('has every page an admin had in the old top bar', () => {
    const tos = new Set([...groupsFor('admin').flatMap((g) => g.items.map((i) => i.to)), ...bottomFor('admin').map((i) => i.to)])
    for (const to of ['/', '/assets', '/databases', '/brokers', '/cloud', '/websites', '/networks', '/approvals', '/teams', '/sessions', '/audit', '/settings', '/about']) {
      expect(tos.has(to), to).toBe(true)
    }
    expect(groupsFor('admin').map((g) => g.label)).toEqual(['', 'Connect', 'Monitoring', 'Networks', 'Cloud', 'People', 'Records'])
  })

  it('gives every entry an icon', () => {
    for (const i of [...groupsFor('owner').flatMap((g) => g.items), ...bottomFor('owner')]) expect(i.icon, i.label).toBeTruthy()
  })

  it('shows an operator only what is theirs', () => {
    const labels = groupsFor('operator').flatMap((g) => g.items.map((i) => i.label))
    expect(labels).toContain('Assets')
    expect(labels).toContain('My sessions')
    expect(labels).not.toContain('Audit log')
    expect(labels).not.toContain('Resources')
  })

  it('lights the entry for the tab on screen, and the page entry on any other', () => {
    const cloud = groupsFor('admin').find((g) => g.label === 'Cloud')!.items
    const lit = (path: string, tab?: string) => cloud.filter((i) => isItemActive(path, tab, i, cloud)).map((i) => i.label)
    expect(lit('/cloud')).toEqual(['Overview'])
    expect(lit('/cloud', 'disks')).toEqual(['Resources'])
    expect(lit('/cloud', 'reservations')).toEqual(['Costs'])
    expect(lit('/cloud', 'access')).toEqual(['Console access'])
    expect(lit('/cloud/connect')).toEqual(['Overview'])
    const nets = groupsFor('admin').find((g) => g.label === 'Networks')!.items
    const litNet = (tab?: string) => nets.filter((i) => isItemActive('/networks', tab, i, nets)).map((i) => i.label)
    expect(litNet()).toEqual(['VPNs and tunnels'])
    expect(litNet('wireguard')).toEqual(['WireGuard'])
    expect(litNet('staff')).toEqual(['VPNs and tunnels'])
    const people = groupsFor('admin').find((g) => g.label === 'People')!.items
    expect(people.filter((i) => isItemActive('/teams', 'people', i, people)).map((i) => i.label)).toEqual(['People'])
    expect(people.filter((i) => isItemActive('/teams', undefined, i, people)).map((i) => i.label)).toEqual(['Teams'])
  })
})

describe('layout choices', () => {
  it('keeps only values the console knows', () => {
    expect(cleanPrefs({ 'assets.view': 'table', sidebar: 'collapsed' })).toEqual({ 'assets.view': 'table', sidebar: 'collapsed' })
    expect(cleanPrefs({ 'assets.view': 'grid', sidebar: 7 })).toEqual({ 'assets.view': 'cards', sidebar: 'expanded' })
    expect(cleanPrefs(undefined)).toEqual({ 'assets.view': 'cards', sidebar: 'expanded' })
  })
})
