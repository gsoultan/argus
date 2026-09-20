import {
  IconActivity, IconClipboardCheck, IconFileDescription, IconPlugConnected,
  IconServer2, IconSettings, IconShieldLock, IconTerminal2, IconUsers,
  type IconProps,
} from '@tabler/icons-react'
import type { ComponentType } from 'react'

/**
 * The console's information architecture, in one place.
 *
 * Nine destinations in a single flat list gave no hint that Connect and Audit
 * log are used by different people for different reasons, so every visit meant
 * reading all nine. Grouping them into three answers — what am I doing, what do
 * we run, who is allowed and what happened — turns that into picking a section
 * first and one of two to four items second.
 *
 * Paths are unchanged. This is a reading order, not a routing change: every
 * bookmark, deep link and test that pointed at a page still does.
 */

export interface NavItem {
  to: string
  label: string
  icon: ComponentType<IconProps>
  /** Shown in the sidebar tooltip — what the page answers, not what it lists. */
  blurb: string
  badge?: (s: NavCounts) => number
  /** `sky` for work in progress, `amber` for work waiting on a person. */
  badgeColor?: 'sky' | 'amber'
}

export interface NavCounts {
  requestsPending: number
  sessionsActive: number
  coverageGaps: number
}

export interface NavSection {
  label: string
  items: NavItem[]
}

export const NAV_SECTIONS: NavSection[] = [
  {
    label: 'Operate',
    items: [
      {
        to: '/',
        label: 'Overview',
        icon: IconActivity,
        blurb: 'Fleet posture and everything in flight right now',
      },
      {
        to: '/connect',
        label: 'Connect',
        icon: IconPlugConnected,
        blurb: 'Open a brokered session from the browser',
      },
      {
        to: '/sessions',
        label: 'Sessions',
        icon: IconTerminal2,
        blurb: 'Every connection, live and historical, with its recording',
        badge: (s) => s.sessionsActive,
        badgeColor: 'sky',
      },
      {
        to: '/requests',
        label: 'Access requests',
        icon: IconClipboardCheck,
        blurb: 'Time-bounded grants waiting on a decision',
        badge: (s) => s.requestsPending,
        badgeColor: 'amber',
      },
    ],
  },
  {
    label: 'Fleet',
    items: [
      {
        to: '/assets',
        label: 'Assets',
        icon: IconServer2,
        blurb: 'Every host Argus can broker a session to',
      },
      {
        to: '/coverage',
        label: 'Coverage',
        icon: IconShieldLock,
        // Badged on the unreviewed count: a coverage gap nobody is told about
        // is one nobody closes.
        blurb: 'Whether Argus can actually see every privileged host',
        badge: (s) => s.coverageGaps,
        badgeColor: 'amber',
      },
    ],
  },
  {
    label: 'Governance',
    items: [
      {
        to: '/audit',
        label: 'Audit log',
        icon: IconFileDescription,
        blurb: 'Hash-chained record of every privileged action',
      },
      {
        to: '/users',
        label: 'Users & roles',
        icon: IconUsers,
        blurb: 'Who holds which role, and separation of duty',
      },
      {
        to: '/settings',
        label: 'Settings',
        icon: IconSettings,
        blurb: 'Gateway policy and what loosening each control costs',
      },
    ],
  },
]

/** True when `pathname` is inside `to` — `/` matches only itself. */
export function isActive(pathname: string, to: string): boolean {
  return to === '/' ? pathname === '/' : pathname.startsWith(to)
}
