import { infiniteQueryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { consoleAlerts, isConfigured, markAlertsRead, saveAlertPrefs, type AlertKind, type AlertPage, type AlertPrefs } from '~/lib/live'
import { alertKeys } from '~/lib/alerts'
import { fixtureMarkRead, fixturePage, fixtureSetPrefs } from '~/lib/alertsFixture'

// What only the opened bell needs: loaded with its panel.

export const ALERT_KINDS: { kind: AlertKind; label: string; description: string; who: string }[] = [
  { kind: 'approval', label: 'Approval requests',
    description: 'Someone asks for access you can decide, or a login waits for one. Also when your own request is decided.',
    who: 'Approvers, admins' },
  { kind: 'signin', label: 'Console sign-ins', description: 'Someone signs in to Argus.', who: 'Admins, auditors' },
  { kind: 'session', label: 'Sessions opened',
    description: 'A terminal, desktop or database session starts, through Argus or around it.', who: 'Admins, auditors' },
  { kind: 'security', label: 'Security alerts',
    description: 'A recovery code used, an authenticator reset, sign-ins refused, "this wasn\'t me".',
    who: 'Admins, auditors' },
  { kind: 'server', label: 'Servers', description: 'A server joins Argus, or its agent is uninstalled.', who: 'Admins, auditors' },
  { kind: 'backup', label: 'Backups',
    description: 'A test restore fails, none has passed for too long, or scheduled backups fail or are missed three times in a row.',
    who: 'Admins, auditors' },
]

/* ── Fixture ─────────────────────────────────────────────────────────────── */

// Without a control plane the bell shows these, under the same "sample data"

/** Every alert, newest first, a page at a time. */
export const allAlertsQuery = () =>
  infiniteQueryOptions({
    queryKey: alertKeys.list(false),
    queryFn: ({ pageParam }) =>
      isConfigured() ? consoleAlerts(false, pageParam || undefined) : Promise.resolve(fixturePage(false)),
    initialPageParam: 0,
    getNextPageParam: (last: AlertPage) => (last.more ? last.alerts[last.alerts.length - 1]?.seq : undefined),
  })

export function useMarkAlertsRead() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: async (input: { seqs?: number[]; upTo?: number }) => {
      if (isConfigured()) return markAlertsRead(input)
      fixtureMarkRead(input)
    },
    onSuccess: () => qc.invalidateQueries({ queryKey: ['alerts', 'list'] }),
  })
}

export function useSaveAlertPrefs() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: async (p: AlertPrefs) => (isConfigured() ? saveAlertPrefs(p) : fixtureSetPrefs(p)),
    onSuccess: (p) => {
      qc.setQueryData(alertKeys.prefs, p)
      void qc.invalidateQueries({ queryKey: ['alerts', 'list'] })
    },
  })
}

export { desktopPermission, desktopWanted, setDesktopWanted } from '~/lib/alertStream'
