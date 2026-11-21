import {
  IconCirclePlus, IconClipboardCheck, IconCloud, IconDatabase, IconHistory, IconHome, IconInfoCircle, IconServer2, IconSettings, IconStack2,
  IconUsersGroup, IconWorld,
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

/** A heading in the sidebar and the pages under it. */
export interface NavGroup {
  /** Empty for the unheaded group at the top (Home, Approvals). */
  label: string
  items: (NavItem & { search?: Record<string, string> })[]
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
 * plans/general-layout/design.md). Every page that exists today is here; a
 * group with nothing this role may see is left out.
 */
export function groupsFor(role: UserRole | undefined): NavGroup[] {
  const by = (to: string) => NAV_ITEMS.find((i) => i.to === to)!
  const g: NavGroup[] = [
    { label: '', items: [by('/'), by('/approvals')] },
    { label: 'Connect', items: [by('/assets'), by('/databases'), by('/brokers')] },
    { label: 'Monitoring', items: [{ ...by('/websites'), label: 'Websites' }] },
    { label: 'Cloud', items: [
      { ...by('/cloud'), label: 'Overview' },
      { ...by('/cloud'), label: 'Resources', search: { tab: 'instances' } },
      { ...by('/cloud'), label: 'Costs', search: { tab: 'costs' } },
      { ...by('/cloud'), label: 'Schedules', search: { tab: 'schedules' } },
      { ...by('/cloud'), label: 'Console access', search: { tab: 'access' } },
    ] },
    { label: 'People', items: [
      { ...by('/teams'), label: 'People', search: { tab: 'people' } },
      { ...by('/teams'), label: 'Teams' },
    ] },
    { label: 'Records', items: [
      { ...by('/activity'), label: seesWholeFleet(role) ? 'Sessions' : 'My sessions' },
      { ...by('/activity'), to: '/audit', label: 'Audit log', audience: 'fleet' },
    ] },
  ]
  return g
    .map((x) => ({ ...x, items: x.items.filter((i) => canSee(role, i.audience)) }))
    .filter((x) => x.items.length > 0)
}
