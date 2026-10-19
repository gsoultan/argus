import {
  IconCirclePlus, IconClipboardCheck, IconFileDescription, IconHome2, IconPlugConnected,
  IconServer2, IconServerBolt, IconSettings, IconShieldCheck, IconTerminal2, IconUsers,
  type IconProps,
} from '@tabler/icons-react'
import type { ComponentType } from 'react'
import { canSee, type Audience } from '~/lib/roles'
import type { UserRole } from '~/types/domain'

/**
 * The console's menu, in one place.
 *
 * Built for someone who has never seen Argus before. The second version still
 * had ten items under three headings, two of them near-synonyms ("All servers"
 * and "Add a server"), and Connect sitting in the list as one page among many
 * when it is the reason most people open the console. Now:
 *
 * - **Connect to a server** is a button at the top, not a list item.
 * - **Four everyday pages**, each with one line saying what it is for, so
 *   nobody has to open a page to learn what it holds.
 * - **Admin** is folded away until it is wanted. It opens by itself when you
 *   are on one of its pages, and shows a count when something in it needs you.
 *
 * Adding a server is a button on the Servers page and the Overview, rather than
 * a menu entry of its own: it is something you do to the list of servers, not a
 * separate place. Paths are unchanged, so every bookmark and deep link works.
 *
 * **The menu follows the role.** An operator was shown the same ten entries as
 * an owner -- fleet posture, the whole inventory, coverage, users -- and had to
 * work out which of them were about the two servers they could actually use.
 * Now someone who is only given servers sees *My servers* and nothing else --
 * an approver also keeps Access requests, which is their job -- and Users &
 * roles is an administrator's alone. The control plane still scopes every list
 * on its own; the routes behind these entries also send a role that is not
 * their audience back to My servers (`guard` in lib/roles.ts).
 */

export interface NavItem {
  to: string
  label: string
  icon: ComponentType<IconProps>
  /** Shown under the label: what the page is for, in a few plain words. */
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

/** The one thing most visits are for, drawn as a button above the menu. */
export const PRIMARY_ACTION: NavItem = {
  to: '/connect',
  label: 'Connect to a server',
  icon: IconPlugConnected,
  blurb: 'Open SSH or Remote Desktop in this browser',
}

export const NAV_SECTIONS: NavSection[] = [
  {
    items: [
      {
        to: '/',
        label: 'Overview',
        icon: IconHome2,
        blurb: 'What needs you now',
        // Someone who is only given servers lands on My servers instead.
        audience: 'fleet',
      },
      {
        to: '/mine',
        label: 'My servers',
        icon: IconServerBolt,
        blurb: 'Servers you can connect to',
      },
      {
        to: '/assets',
        label: 'Servers',
        icon: IconServer2,
        blurb: 'Add and manage servers',
        audience: 'fleet',
        // Servers whose agent checked in and that nobody has approved yet.
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
  { ...PRIMARY_ACTION, audience: 'fleet' },
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
