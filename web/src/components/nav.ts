import {
  IconBox, IconCalendarTime, IconCirclePlus, IconClipboardCheck, IconCloud, IconCoin, IconDatabase, IconFileText, IconHistory, IconHome,
  IconInfoCircle, IconKey, IconRouter, IconTopologyStar3, IconServer2, IconSettings, IconStack2, IconUser, IconUsersGroup, IconWorld,
  type IconProps,
} from '@tabler/icons-react'
import type { ComponentType } from 'react'
import { canSee, seesWholeFleet, type Audience } from '~/lib/roles'
import type { UserRole } from '~/types/domain'

/**
 * The console's menu, in one place.
 *
 * Six words across the top, and nothing else:
 *
 * - **Home**: where the console opens. What is waiting on you, then the fleet
 *   at a glance for whoever sees all of it, or your own servers otherwise.
 * - **Assets**: every server, workstation and device you can open, with
 *   Connect on each. An admin
 *   also adds and manages them here.
 * - **Databases**: every database you can open, in a query workspace. Listed
 *   next to Servers because people think "open orders", not "open core-db-01".
 * - **Cloud**: the connected AWS accounts and what is in them, and which
 *   instances have an Argus server. For whoever sees the whole fleet.
 * - **Websites**: whether each site is up, and its uptime. For whoever sees
 *   the whole fleet; admins add the addresses.
 * - **Network devices**: switches, routers and the like, read over SNMP by a
 *   server near them. Monitoring only: they are not Assets.
 * - **Networks**: VPN links and tunnels on the servers, as their agents see
 *   them. For whoever sees the whole fleet.
 * - **Approvals**: everything waiting on a person. Logins straight to a server
 *   that the rule holds, root asked for, and access asked for. It is a menu
 *   item of its own, with a count, because someone is waiting at a terminal.
 * - **Teams**: teams and the people in them. A server is given to a team.
 * - **Activity**: who connected where, the recordings, and the audit log.
 * - **Settings**: the rules for sessions and sign-in.
 *
 * Someone who is only given servers sees Servers and My activity. An approver
 * also gets Approvals, which is their job. The control plane scopes every list
 * on its own; the routes behind these entries also send a role that is not
 * their audience back to Servers (`guard` in lib/guard.ts).
 */

export interface NavItem {
  to: string
  label: string
  icon: ComponentType<IconProps>
  /** What the page is for, in a few plain words. Shown in the command palette. */
  blurb: string
  badge?: (s: NavCounts) => number
  /** Who it is shown to; see `Audience`. Omitted means everyone. */
  audience?: Audience
}

/** One entry in the sidebar: a page, or a tab of one. */
export interface SideItem extends NavItem {
  /** The search the link sets, for an entry that opens a tab. */
  search?: Record<string, string>
  /**
   * The ?tab= values this entry stands for. An entry with none is the page
   * itself, lit when no sibling's tab is open.
   */
  tabs?: string[]
}

/** A heading in the sidebar and the pages under it. */
export interface NavGroup {
  /** Empty for the unheaded group at the top (Home, Approvals). */
  label: string
  items: SideItem[]
}

export interface NavCounts {
  /** Everything on Approvals: held logins and access requests. */
  approvalsPending: number
  /** Servers whose agent checked in and that nobody has finished adding. */
  serversWaiting: number
}

export const NAV_ITEMS: NavItem[] = [
  {
    to: '/',
    label: 'Home',
    icon: IconHome,
    blurb: 'What is waiting on you, and what is happening now',
  },
  {
    to: '/assets',
    label: 'Assets',
    icon: IconServer2,
    blurb: 'Connect to a server, workstation or device, or add one',
    badge: (s) => s.serversWaiting,
  },
  {
    to: '/databases',
    label: 'Databases',
    icon: IconDatabase,
    blurb: 'Open a database in the browser; every statement is recorded',
  },
  {
    to: '/brokers',
    label: 'Brokers',
    icon: IconStack2,
    blurb: 'RabbitMQ: queues, routing and who is connected; every action is recorded',
  },
  {
    to: '/cloud',
    label: 'Cloud',
    icon: IconCloud,
    blurb: 'AWS accounts: instances, disks and firewalls, and what has no Argus server',
    audience: 'fleet',
  },
  {
    to: '/websites',
    label: 'Websites',
    icon: IconWorld,
    blurb: 'Is each website up, and its uptime; confirmed before anyone is told it is down',
    audience: 'fleet',
  },
  {
    to: '/network-devices',
    label: 'Network devices',
    icon: IconRouter,
    blurb: 'Switches, routers, firewalls and printers over SNMP: up, restarted, and their interfaces',
    audience: 'fleet',
  },
  {
    to: '/networks',
    label: 'Networks',
    icon: IconTopologyStar3,
    blurb: 'VPN links and Cloudflare Tunnels on your servers: up or down, and what each carries',
    audience: 'fleet',
  },
  {
    to: '/approvals',
    label: 'Approvals',
    icon: IconClipboardCheck,
    blurb: 'Logins and access waiting on you',
    audience: 'approvers',
    badge: (s) => s.approvalsPending,
  },
  {
    to: '/teams',
    label: 'Teams',
    icon: IconUsersGroup,
    blurb: 'Teams and the people in them',
    audience: 'fleet',
  },
  {
    to: '/activity',
    label: 'Activity',
    icon: IconHistory,
    blurb: 'Who connected where, and recordings',
  },
  {
    to: '/settings',
    label: 'Settings',
    icon: IconSettings,
    blurb: 'Rules for sessions and sign-in',
    audience: 'fleet',
  },
]

/**
 * Every destination, for the command palette: the menu, plus the pages reached
 * by a button rather than a menu item.
 */
export const ALL_PAGES: NavItem[] = [
  ...NAV_ITEMS,
  {
    to: '/assets/add',
    label: 'Add an asset',
    icon: IconCirclePlus,
    blurb: 'Install the agent and say who can use it',
    audience: 'admin',
  },
  {
    to: '/about',
    label: 'About Argus',
    icon: IconInfoCircle,
    blurb: 'Version, description and copyright',
  },
]

/** The palette's pages for this role, by the same rule as the menu. */
export function pagesFor(role: UserRole | undefined): NavItem[] {
  return ALL_PAGES.filter((p) => canSee(role, p.audience))
}

/**
 * The menu as this role should see it. An unknown role gets the narrowest.
 * Activity is "My activity" for someone who only sees their own sessions.
 */
export function navFor(role: UserRole | undefined): NavItem[] {
  return NAV_ITEMS.filter((i) => canSee(role, i.audience)).map((i) =>
    i.to === '/activity' && !seesWholeFleet(role) ? { ...i, label: 'My activity' } : i,
  )
}

/** True when `pathname` is `to` or a page below it. `/` matches only itself. */
export function isActive(pathname: string, to: string): boolean {
  if (to === '/') return pathname === '/'
  if (to === '/activity' && (pathname.startsWith('/sessions') || pathname === '/audit')) return true
  return pathname === to || pathname.startsWith(to + '/')
}

/**
 * The sidebar: six groups, each with one rule for what goes in it (see
 * plans/general-layout/design.md), and a plain icon for every entry so the
 * menu still reads when it is folded to icons. Every page that exists today
 * is here; a group with nothing this role may see is left out.
 *
 * - Connect: what you open a session to.
 * - Monitoring: is it up, and who is told when it is not.
 * - Networks: VPN links and tunnels between servers, sites and clouds.
 * - Cloud: the connected cloud accounts.
 * - People: who can sign in, and the teams things are given to.
 * - Records: what happened: sessions and the audit log.
 * Settings and About sit apart at the bottom (bottomFor).
 */
export function groupsFor(role: UserRole | undefined, seesChecks = false): NavGroup[] {
  const by = (to: string) => NAV_ITEMS.find((i) => i.to === to)!
  const cloud = by('/cloud')
  const teams = by('/teams')
  const g: NavGroup[] = [
    { label: '', items: [by('/'), by('/approvals')] },
    { label: 'Connect', items: [by('/assets'), by('/databases'), by('/brokers')] },
    // Someone outside the fleet roles who was given a check sees it here.
    { label: 'Monitoring', items: [
      { ...by('/websites'), label: 'Websites', ...(seesChecks ? { audience: undefined } : {}) },
      { ...by('/network-devices'), ...(seesChecks ? { audience: undefined } : {}) },
    ] },
    { label: 'Networks', items: [{ ...by('/networks'), label: 'VPNs and tunnels' }] },
    { label: 'Cloud', items: [
      { ...cloud, label: 'Overview' },
      { ...cloud, label: 'Resources', icon: IconBox, search: { tab: 'instances' },
        tabs: ['instances', 'disks', 'network', 'scaling', 'risks', 'changes'], blurb: 'Instances, disks, networks and scale groups' },
      { ...cloud, label: 'Costs', icon: IconCoin, search: { tab: 'costs' }, tabs: ['costs', 'reservations'], blurb: 'What the cloud accounts cost' },
      { ...cloud, label: 'Schedules', icon: IconCalendarTime, search: { tab: 'schedules' }, tabs: ['schedules'], blurb: 'When instances start and stop' },
      { ...cloud, label: 'Console access', icon: IconKey, search: { tab: 'access' }, tabs: ['access'], blurb: 'Who can sign in to the cloud consoles' },
    ] },
    { label: 'People', items: [
      { ...teams, label: 'People', icon: IconUser, search: { tab: 'people' }, tabs: ['people'], blurb: 'Everyone who can sign in' },
      { ...teams, label: 'Teams' },
    ] },
    { label: 'Records', items: [
      { ...by('/activity'), to: '/sessions', label: seesWholeFleet(role) ? 'Sessions' : 'My sessions' },
      { ...by('/activity'), to: '/audit', label: 'Audit log', icon: IconFileText, audience: 'fleet', blurb: 'Every change, who made it and when' },
    ] },
  ]
  return g
    .map((x) => ({ ...x, items: x.items.filter((i) => canSee(role, i.audience)) }))
    .filter((x) => x.items.length > 0)
}

/** Settings and About, kept apart at the foot of the sidebar. */
export function bottomFor(role: UserRole | undefined): SideItem[] {
  return [
    NAV_ITEMS.find((i) => i.to === '/settings')!,
    ALL_PAGES.find((i) => i.to === '/about')!,
  ].filter((i) => canSee(role, i.audience))
}

/**
 * Whether a sidebar entry is the page on screen. An entry for a tab is lit on
 * that tab; the page's own entry is lit on any tab no sibling claims.
 */
export function isItemActive(pathname: string, tab: string | undefined, item: SideItem, siblings: SideItem[]): boolean {
  if (!isActive(pathname, item.to)) return false
  // Sessions and the audit log share Records; each is its own page.
  if (item.to === '/sessions') return pathname.startsWith('/sessions') || pathname === '/activity'
  const here = pathname === item.to
  if (item.tabs) return here && tab !== undefined && item.tabs.includes(tab)
  const claimed = siblings.some((s) => s !== item && s.to === item.to && s.tabs && tab !== undefined && s.tabs.includes(tab))
  return !(here && claimed)
}
