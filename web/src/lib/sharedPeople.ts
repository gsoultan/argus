/*
 * Where people work and what they can reach: the calls, queries and mutations
 * behind the People page's "What they can do" and "Organizations" windows and
 * adding someone from another organization. Apart from live.ts and
 * queries.ts, which every page loads, so only the People page pays for them.
 */
import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { api } from '~/lib/api'
import { get, isConfigured, post, send } from '~/lib/live'
import { qk } from '~/lib/queries'
import type { AccessSection, ManagedOrganization, PersonOrganizations } from '~/types/domain'

const person = (email: string) => `/api/v1/users/${encodeURIComponent(email)}`

/** Calls with a demo fallback; an object so tests can stub one. */
export const sharedPeopleApi = {
  /** People of the other organizations the caller runs who are not here yet; none in the demo. */
  async fromOrganizations(): Promise<{ organizations: ManagedOrganization[] }> {
    if (isConfigured()) return get('/api/v1/users/from-organizations')
    return { organizations: [] }
  },

  /**
   * Everything one person may reach, one section per kind of thing. The demo
   * has servers only, built from the same assignments as personServers.
   */
  async personAccess(email: string): Promise<AccessSection[]> {
    if (isConfigured()) return get<AccessSection[]>(`${person(email)}/access`)
    const servers = await api.personServers(email)
    const empty = (kind: string, label: string, give: string, noun: string): AccessSection =>
      ({ kind, label, give, items: [], empty: `No ${noun} yet. Give one to a team of theirs, or to them, on its page.` })
    return [
      {
        kind: 'servers', label: 'Servers', give: '/assets',
        empty: "No servers yet. Give one to a team of theirs, or to them, on the server's page.",
        items: servers.map((s) => ({
          id: s.assetId, name: s.hostname, own: s.ownSetting, teams: s.ownSetting ? [] : s.teams,
          what: [
            s.personalAccount ? `their own account${s.sudo ? ' with sudo' : ''}` : '',
            s.principals.length ? `as ${s.principals.join(', ')}` : '',
            s.personalAccount && s.directSsh === 'allow' ? 'direct SSH' : '',
            s.personalAccount && s.directSsh === 'approve' ? 'direct SSH after approval' : '',
          ].filter(Boolean).join(' · ') || 'no access of their own here',
        })),
      },
      empty('databases', 'Databases', '/databases', 'databases'),
      empty('brokers', 'Brokers', '/brokers', 'brokers'),
      empty('cloud', 'Cloud', '/cloud', 'cloud access'),
    ]
  },
}

/** Everything one person may reach here, by kind. */
export const personAccessQuery = (email: string) =>
  queryOptions({ queryKey: ['person-access', email], queryFn: () => sharedPeopleApi.personAccess(email) })

/** Where one of our own people works; only home in the demo. */
export const personOrganizationsQuery = (email: string) =>
  queryOptions({
    queryKey: ['person-organizations', email],
    queryFn: () => isConfigured()
      ? get<PersonOrganizations>(`${person(email)}/organizations`)
      : Promise.resolve({ home: { slug: 'default', name: 'Default organization', role: '' }, organizations: [], canAddTo: [], wasShareable: false }),
  })

/** People of the other organizations the caller runs who are not here yet. */
export const fromOrganizationsQuery = () =>
  queryOptions({ queryKey: ['from-organizations'], queryFn: () => sharedPeopleApi.fromOrganizations() })

/**
 * Allows an organization to have one of our people. With a role, and when the
 * caller runs that organization too, adds them there in the same step.
 */
export function useAllowPersonOrganization() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { email: string; org: string; role?: string }): Promise<{ org: string; allowed: true; added: boolean }> =>
      post(`${person(v.email)}/organizations`, v.role ? { org: v.org, role: v.role } : { org: v.org }),
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: ['person-organizations', v.email] })
      void qc.invalidateQueries({ queryKey: qk.users })
      void qc.invalidateQueries({ queryKey: qk.me })
    },
  })
}

/** Takes an organization away from one of our people; they are refused there at once. */
export function useRemovePersonOrganization() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: async (v: { email: string; org: string }) => {
      await send('DELETE', `${person(v.email)}/organizations/${encodeURIComponent(v.org)}`)
    },
    onSuccess: (_d, v) => {
      void qc.invalidateQueries({ queryKey: ['person-organizations', v.email] })
      void qc.invalidateQueries({ queryKey: qk.users })
    },
  })
}
