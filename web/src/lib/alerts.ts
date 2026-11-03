import { queryOptions } from '@tanstack/react-query'
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
  | { type: 'ready' | 'read' }
  | { type: 'end' }

/** Whether an alert is one someone has to act on, which earns the chime. */
export function urgent(a: ConsoleAlert): boolean {
  return a.kind === 'security' || (a.kind === 'approval' && a.severity !== 'info')
}
