import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { get, isConfigured, send } from '~/lib/live'

/*
 * How each person likes the console laid out: cards or a table on Assets, the
 * sidebar open or folded to icons. Kept on the control plane per person
 * (GET/PUT /api/v1/me/preferences), so the latest choice is the default at
 * the next sign-in, in any browser. A copy in this browser's storage lets the
 * first paint use it before the control plane answers; with no control plane
 * (the sample console) that copy is all there is.
 */

export interface UIPrefs {
  'assets.view': 'cards' | 'table'
  sidebar: 'expanded' | 'collapsed'
}

export const DEFAULT_PREFS: UIPrefs = { 'assets.view': 'cards', sidebar: 'expanded' }

const LOCAL = 'argus-ui-preferences'

function readLocal(): Partial<UIPrefs> {
  try {
    const raw = localStorage.getItem(LOCAL)
    return raw ? (JSON.parse(raw) as Partial<UIPrefs>) : {}
  } catch {
    return {}
  }
}

function writeLocal(p: UIPrefs) {
  try {
    localStorage.setItem(LOCAL, JSON.stringify(p))
  } catch {
    // Storage refused (a private window): the control plane still has it.
  }
}

/** Only values this console knows; anything else falls back to the default. */
export function cleanPrefs(raw: Partial<Record<string, unknown>> | undefined): UIPrefs {
  const view = raw?.['assets.view']
  const side = raw?.sidebar
  return {
    'assets.view': view === 'table' || view === 'cards' ? view : DEFAULT_PREFS['assets.view'],
    sidebar: side === 'collapsed' || side === 'expanded' ? side : DEFAULT_PREFS.sidebar,
  }
}

export const prefsQuery = () => ({
  queryKey: ['ui-preferences'] as const,
  queryFn: async (): Promise<UIPrefs> => {
    if (!isConfigured()) return cleanPrefs(readLocal())
    const p = cleanPrefs(await get<Partial<UIPrefs>>('/api/v1/me/preferences'))
    writeLocal(p)
    return p
  },
  // The browser's copy first, so the page does not draw cards and then
  // jump to the table someone chose.
  placeholderData: () => cleanPrefs(readLocal()),
  staleTime: 5 * 60_000,
})

/** One layout choice and a setter that keeps it for next time. */
export function usePreference<K extends keyof UIPrefs>(key: K): [UIPrefs[K], (v: UIPrefs[K]) => void] {
  const qc = useQueryClient()
  const { data } = useQuery(prefsQuery())
  const save = useMutation({
    mutationFn: ({ k, v }: { k: K; v: UIPrefs[K] }) =>
      isConfigured() ? send<Partial<UIPrefs>>('PUT', '/api/v1/me/preferences', { key: k, value: v }) : Promise.resolve(undefined),
    onSuccess: (saved) => {
      if (saved) qc.setQueryData(prefsQuery().queryKey, cleanPrefs(saved))
    },
  })
  const value = (data ?? cleanPrefs(readLocal()))[key]
  const set = (v: UIPrefs[K]) => {
    const next = { ...(qc.getQueryData<UIPrefs>(prefsQuery().queryKey) ?? cleanPrefs(readLocal())), [key]: v }
    // Shown at once; the control plane is told after.
    qc.setQueryData(prefsQuery().queryKey, next)
    writeLocal(next)
    save.mutate({ k: key, v })
  }
  return [value, set]
}
