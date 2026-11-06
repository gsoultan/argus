import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  assetPackages,
  checkPackages,
  decidePackageRun,
  isConfigured,
  packageFleet,
  packagePolicy,
  packageRun,
  runFinished,
  savePackagePolicy,
  startPackageRun,
  startPackageRuns,
  type PackagePolicy,
  type PackageReboot,
} from '~/lib/live'

/*
 * Operating system updates. Live only: there is no sample of a server's
 * package list worth showing as if it were real, so against no control plane
 * these queries do not run and the views say they need one.
 */

const viewKey = (assetId: string) => ['packages', 'asset', assetId] as const
const fleetKey = ['packages', 'fleet'] as const
const policyKey = ['packages', 'policy'] as const
const runKey = (id: string) => ['packages', 'run', id] as const

/** Fast while a look or a run is going, so its steps show as they happen. */
export const assetPackagesQuery = (assetId: string) =>
  queryOptions({
    queryKey: viewKey(assetId),
    queryFn: () => assetPackages(assetId),
    enabled: isConfigured(),
    refetchInterval: (q) => {
      const v = q.state.data
      if (!v) return false
      const moving = v.checking || v.checkAsked || v.runs.some((r) => !runFinished(r.state))
      return moving ? 3_000 : 60_000
    },
  })

export const packageRunQuery = (id: string, enabled = true) =>
  queryOptions({
    queryKey: runKey(id),
    queryFn: () => packageRun(id),
    enabled: isConfigured() && enabled && id !== '',
    refetchInterval: (q) => (q.state.data && !runFinished(q.state.data.state) ? 3_000 : false),
  })

export const packageFleetQuery = () =>
  queryOptions({
    queryKey: fleetKey,
    queryFn: packageFleet,
    enabled: isConfigured(),
    refetchInterval: (q) => (q.state.data?.some((r) => r.run !== null) ? 5_000 : 120_000),
  })

export const packagePolicyQuery = () =>
  queryOptions({ queryKey: policyKey, queryFn: packagePolicy, enabled: isConfigured() })

function useInvalidate() {
  const qc = useQueryClient()
  return () => void qc.invalidateQueries({ queryKey: ['packages'] })
}

export function useCheckPackages(assetId: string) {
  const done = useInvalidate()
  return useMutation({ mutationFn: () => checkPackages(assetId), onSettled: done })
}

export function useStartPackageRun(assetId: string) {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (body: { mode: 'all' | 'security' | 'selected'; packages?: string[]; reboot: PackageReboot }) =>
      startPackageRun(assetId, body),
    onSettled: done,
  })
}

export function useStartPackageRuns() {
  const done = useInvalidate()
  return useMutation({ mutationFn: startPackageRuns, onSettled: done })
}

export function useDecidePackageRun() {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (v: { id: string; action: 'approve' | 'deny' | 'cancel' }) => decidePackageRun(v.id, v.action),
    onSettled: done,
  })
}

export function useSavePackagePolicy() {
  const done = useInvalidate()
  return useMutation({ mutationFn: (p: PackagePolicy) => savePackagePolicy(p), onSettled: done })
}
