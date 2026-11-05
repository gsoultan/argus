import { queryOptions, type QueryClient, type QueryKey } from '@tanstack/react-query'
import {
  alertPrefs, consoleAlerts, isConfigured,
  type AlertKind, type AlertPage, type AlertPrefs, type ConsoleAlert,
} from '~/lib/live'

/**
 * The bell in the console header: its queries, and the one live stream that
 * keeps them current.
 *
 * The stream says that something changed; the list and the unread count are
 * always fetched. So an event the stream missed, across a reconnect or from
 * another replica, costs a pop-up and never a wrong badge.
 */

export type { AlertKind, AlertPage, AlertPrefs, ConsoleAlert }

export const alertKeys = {
  all: ['alerts'] as const,
  list: (unread: boolean) => ['alerts', 'list', unread] as const,
  prefs: ['alerts', 'prefs'] as const,
}

const fixture = () => import('~/lib/alertsFixture')

/** The unread alerts: the badge, and the first tab. */
export const unreadAlertsQuery = () =>
  queryOptions({
    queryKey: alertKeys.list(true),
    queryFn: async () => (isConfigured() ? consoleAlerts(true) : (await fixture()).fixturePage(true)),
    // The stream is what keeps this current. The interval is the net under
    // it, for a proxy that holds events back or a stream that never opened.
    refetchInterval: 120_000,
  })

export const alertPrefsQuery = () =>
  queryOptions({
    queryKey: alertKeys.prefs,
    queryFn: async () => (isConfigured() ? alertPrefs() : (await fixture()).fixtureGetPrefs()),
    staleTime: 5 * 60_000,
  })

export type StreamStatus = 'fixture' | 'connecting' | 'live' | 'reconnecting' | 'ended'

export type StreamEvent =
  | { type: 'alert'; alert: ConsoleAlert }
  /** Alerts whose request was decided or taken back: nothing to act on now. */
  | { type: 'closed'; seqs: number[] }
  | { type: 'ready' | 'read' }
  | { type: 'end' }

/** Whether an alert is one someone has to act on, which earns the chime. */
export function urgent(a: ConsoleAlert): boolean {
  if (a.closed) return false
  return a.kind === 'security' || (a.kind === 'approval' && a.severity !== 'info')
}

/**
 * The queries behind the page an alert links to, by the link's path.
 *
 * Opening an alert's link while already on that page is a navigation to the
 * same route: nothing remounts, and the loader's `ensureQueryData` hands back
 * what is cached. So the page an alert says has changed kept showing what it
 * had, until someone reloaded it.
 */
export function alertLinkKeys(link: string): QueryKey[] {
  const path = link.split(/[?#]/, 1)[0] ?? ''
  const under = (p: string) => path === p || path.startsWith(`${p}/`)
  if (under('/approvals')) return [['requests'], ['direct-logins']]
  if (under('/audit')) return [['audit']]
  if (under('/assets')) return [['assets'], ['asset']]
  if (under('/databases')) {
    const id = path.split('/')[2]
    return id ? [['databases'], ['database', id]] : [['databases']]
  }
  return []
}

/**
 * Marks what an alert's page shows as stale, so it is fetched again: at once
 * if that page is on screen, and on arrival if it is not.
 */
export function refreshAlertTarget(qc: QueryClient, link: string): void {
  for (const queryKey of alertLinkKeys(link)) void qc.invalidateQueries({ queryKey })
}
