import {
  IconCirclePlus, IconClipboardCheck, IconDatabase, IconHistory, IconInfoCircle, IconServer2, IconSettings,
  IconUsersGroup,
  type IconProps,
} from '@tabler/icons-react'
import type { ComponentType } from 'react'
import { canSee, seesWholeFleet, type Audience } from '~/lib/roles'
import type { UserRole } from '~/types/domain'

/**
 * The console's menu, in one place.
 *
 * Five words across the top, and nothing else:
 *
 * - **Servers**: every server you can open, with Connect on each. An admin
 *   also adds and manages them here.
 * - **Databases**: every database you can open, in a query workspace. Listed
 *   next to Servers because people think "open orders", not "open core-db-01".
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

export interface NavCounts {
  /** Everything on Approvals: held logins and access requests. */
  approvalsPending: number
  /** Servers whose agent checked in and that nobody has finished adding. */
  serversWaiting: number
}

export const NAV_ITEMS: NavItem[] = [
  {
    to: '/assets',
    label: 'Servers',
    icon: IconServer2,
    blurb: 'Connect to a server, or add one',
    badge: (s) => s.serversWaiting,
  },
  {
    to: '/databases',
    label: 'Databases',
    icon: IconDatabase,
    blurb: 'Open a database in the browser; every statement is recorded',
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
    label: 'Add a server',
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
