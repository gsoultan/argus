import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { get, isConfigured, send } from '~/lib/live'

/*
 * Logins while a server cannot reach Argus: members of named groups are let
 * in over plain SSH, still recorded, once its agent has gone this long
 * without hearing from Argus. Off by default; a server can turn it off.
 */

export interface OutageLogins {
  afterMinutes: number
  groups: string[]
  updatedAt?: string
  updatedBy?: string
}

export interface AssetOutageLogins extends OutageLogins {
  off: boolean
}

export const OUTAGE_CHOICES = [
  { value: '0', label: 'Off' },
  { value: '5', label: '5 min' },
  { value: '15', label: '15 min' },
  { value: '60', label: '1 hour' },
] as const

/** Group names as the control plane accepts them. */
export const GROUP_NAME = /^[A-Za-z_][A-Za-z0-9._-]{0,31}$/
export const MAX_GROUPS = 16

/** Why a group list cannot be saved, or null. */
export function outageGroupsError(groups: string[]): string | null {
  if (groups.length === 0) return 'Name at least one group'
  if (groups.length > MAX_GROUPS) return `At most ${MAX_GROUPS} groups`
  const bad = groups.find((g) => !GROUP_NAME.test(g))
  return bad ? `"${bad}" is not a group name` : null
}

/** "15 minutes", "1 hour". */
export function outageAfterLabel(minutes: number): string {
  if (minutes === 60) return '1 hour'
  return `${minutes} minutes`
}

const key = ['policy', 'outage-logins'] as const

/**
 * The answer, if it is one. An older control plane, or a proxy, answering
 * with something else fails the query instead of the page that draws it.
 */
export function checked<T extends { afterMinutes: number; groups: string[] }>(v: T): T {
  if (!v || typeof v !== 'object' || typeof v.afterMinutes !== 'number' || !Array.isArray(v.groups)) {
    throw new Error('argus-control did not say how logins work while it is down')
  }
  return v
}
const assetKey = (id: string) => ['assets', id, 'outage-logins'] as const

export function useOutageLogins() {
  return useQuery({
    queryKey: key,
    queryFn: async () => checked(await get<OutageLogins>('/api/v1/policy/outage-logins')),
    enabled: isConfigured(),
  })
}

export function useSaveOutageLogins() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (o: Pick<OutageLogins, 'afterMinutes' | 'groups'>) =>
      send<OutageLogins>('PUT', '/api/v1/policy/outage-logins', o),
    onSuccess: (o) => {
      qc.setQueryData(key, o)
      void qc.invalidateQueries({ queryKey: ['assets'] })
    },
  })
}

export function useAssetOutageLogins(id: string) {
  return useQuery({
    queryKey: assetKey(id),
    queryFn: async () => checked(await get<AssetOutageLogins>(`/api/v1/assets/${encodeURIComponent(id)}/outage-logins`)),
    enabled: isConfigured(),
  })
}

export function useSetAssetOutageLoginsOff(id: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (off: boolean) =>
      send<AssetOutageLogins>('PUT', `/api/v1/assets/${encodeURIComponent(id)}/outage-logins`, { off }),
    onSuccess: (v) => qc.setQueryData(assetKey(id), v),
  })
}
