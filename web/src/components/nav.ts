import {
  IconCirclePlus, IconClipboardCheck, IconFileDescription, IconHome2, IconServer2,
  IconSettings, IconShieldCheck, IconTerminal2, IconUsers, type IconProps,
} from '@tabler/icons-react'
import type { ComponentType } from 'react'
import { canSee, type Audience } from '~/lib/roles'
import type { UserRole } from '~/types/domain'

/**
 * The console's menu, in one place.
 *
 * The third version still had three ways to the same servers: a "Connect to a
 * server" button, "My servers" and "Servers". People asked what the difference
 * was, and the honest answer was "a form, some cards and a table of the same
 * machines". Now:
 *
 * - **One Servers page** for everyone. Its Connect view is a card per server
 *   with a Connect button; administrators also get a Manage view with the
 *   inventory and the buttons to add and approve servers. /mine redirects
 *   there, and /connect is where a Connect button takes you, not a menu entry.
 * - **One line per item.** Names short enough not to need a caption: the
 *   caption under every item doubled the menu's height and made it read as a
 *   wall of text. What each page is for still shows in the command palette.
 * - **Admin** is folded away until it is wanted. It opens by itself when you
 *   are on one of its pages, and shows a count when something in it needs you.
 *
 * **The menu follows the role.** Someone who is only given servers sees
 * Servers and nothing else -- an approver also keeps Access requests, which is
 * their job -- and Users & roles is an administrator's alone. The control plane
 * still scopes every list on its own; the routes behind these entries also send
 * a role that is not their audience back to Servers (`guard` in lib/guard.ts).
 */

export interface NavItem {
  to: string
  label: string
  icon: ComponentType<IconProps>
  /** What the page is for, in a few plain words. Shown in the command palette. */
  blurb: string
  badge?: (s: NavCounts) => number
  /** `sky` for work in progress, `amber` for work waiting on a person. */
  badgeColor?: 'sky' | 'amber'
  /** Who it is shown to; see `Audience`. Omitted means everyone. */
  audience?: Audience
}

export interface NavCounts {
  requestsPending: number
  sessionsActive: number
  coverageGaps: number
  serversWaiting: number
}

export interface NavSection {
  /** Omitted for the everyday section, which needs no heading. */
  label?: string
  /** Folded away until opened, or until the current page is inside it. */
  collapsible?: boolean
  /** Who the whole section is shown to; see `Audience`. */
  audience?: Audience
  items: NavItem[]
}

export const NAV_SECTIONS: NavSection[] = [
  {
    items: [
      {
        to: '/',
        label: 'Overview',
        icon: IconHome2,
        blurb: 'What needs you now',
        // Someone who is only given servers lands on Servers instead.
        audience: 'fleet',
      },
      {
        to: '/assets',
        label: 'Servers',
        icon: IconServer2,
        blurb: 'Connect to a server, or add and manage them',
        // Servers whose agent checked in and that nobody has approved yet.
        // Zero for anyone who cannot approve them; see the shell.
        badge: (s) => s.serversWaiting,
        badgeColor: 'amber',
      },
      {
        to: '/sessions',
        label: 'Sessions',
        icon: IconTerminal2,
        blurb: 'Live and recorded',
        audience: 'fleet',
        badge: (s) => s.sessionsActive,
        badgeColor: 'sky',
      },
      {
        to: '/requests',
        label: 'Access requests',
        icon: IconClipboardCheck,
        blurb: 'Ask for or grant access',
        audience: 'approvers',
        badge: (s) => s.requestsPending,
        badgeColor: 'amber',
      },
    ],
  },
  {
    label: 'Admin',
    collapsible: true,
    audience: 'fleet',
    items: [
      {
        to: '/coverage',
        label: 'Coverage',
        icon: IconShieldCheck,
        blurb: 'Is every server watched?',
        // A coverage gap nobody is told about is one nobody closes.
        badge: (s) => s.coverageGaps,
        badgeColor: 'amber',
      },
      {
        to: '/audit',
        label: 'Audit log',
        icon: IconFileDescription,
        blurb: 'Who did what, and when',
      },
      {
        to: '/users',
        label: 'Users & roles',
        icon: IconUsers,
        blurb: 'Who can do what',
        audience: 'admin',
      },
      {
        to: '/settings',
        label: 'Settings',
        icon: IconSettings,
        blurb: 'Rules for sessions',
      },
    ],
  },
]

/**
 * Every destination, for the command palette: the menu, plus the pages reached
 * by a button rather than a menu item.
 */
export const ALL_PAGES: NavItem[] = [
  // A section's audience narrows each of its items, so the palette never offers
  // a page the menu hides.
  ...NAV_SECTIONS.flatMap((s) => s.items.map((i) => (s.audience && !i.audience ? { ...i, audience: s.audience } : i))),
  {
    to: '/assets/add',
    label: 'Add a server',
    icon: IconCirclePlus,
    blurb: 'Install the agent and approve the server',
    audience: 'admin',
  },
]

/** The palette's pages for this role, by the same rule as the menu. */
export function pagesFor(role: UserRole | undefined): NavItem[] {
  return ALL_PAGES.filter((p) => canSee(role, p.audience))
}

/** The menu as this role should see it. An unknown role gets the narrowest. */
export function navFor(role: UserRole | undefined): NavSection[] {
  return NAV_SECTIONS.filter((s) => canSee(role, s.audience))
    .map((s) => ({ ...s, items: s.items.filter((i) => canSee(role, i.audience)) }))
    .filter((s) => s.items.length > 0)
}

/** True when `pathname` is `to` or a page below it. `/` matches only itself. */
export function isActive(pathname: string, to: string): boolean {
  if (to === '/') return pathname === '/'
  return pathname === to || pathname.startsWith(to + '/')
}
