/*
 * Growing disks. Live only: a sample server's disks shown as if real would
 * invite growing them, so against no control plane these queries do not run
 * and the views say they need one.
 */
import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  assetDisks, decideDiskRun, diskPolicy, diskRun, diskRunFinished, saveDiskPolicy, scanDisks, startDiskGrow,
  type DiskGrowPolicy,
} from '~/lib/diskApi'
import { isConfigured } from '~/lib/live'

const viewKey = (assetId: string) => ['disks', 'asset', assetId] as const
const policyKey = ['disks', 'policy'] as const
const runKey = (id: string) => ['disks', 'run', id] as const

/** Fast while a scan or a grow is going, so each step shows as it lands. */
export const assetDisksQuery = (assetId: string) =>
  queryOptions({
    queryKey: viewKey(assetId),
    queryFn: () => assetDisks(assetId),
    enabled: isConfigured(),
    refetchInterval: (q) => {
      const v = q.state.data
      if (!v) return false
      const moving = v.scanning || v.scanAsked || v.runs.some((r) => !diskRunFinished(r.state))
      return moving ? 3_000 : 60_000
    },
  })

export const diskRunQuery = (id: string, enabled = true) =>
  queryOptions({
    queryKey: runKey(id),
    queryFn: () => diskRun(id),
    enabled: isConfigured() && enabled && id !== '',
    refetchInterval: (q) => (q.state.data && !diskRunFinished(q.state.data.state) ? 3_000 : false),
  })

export const diskPolicyQuery = () =>
  queryOptions({ queryKey: policyKey, queryFn: diskPolicy, enabled: isConfigured() })

function useInvalidate() {
  const qc = useQueryClient()
  return () => void qc.invalidateQueries({ queryKey: ['disks'] })
}

export function useScanDisks(assetId: string) {
  const done = useInvalidate()
  return useMutation({ mutationFn: (rescan: boolean) => scanDisks(assetId, rescan), onSettled: done })
}

export function useStartDiskGrow(assetId: string) {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (body: { key: string; bytes: number }) => startDiskGrow(assetId, body),
    onSettled: done,
  })
}

export function useDecideDiskRun() {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (v: { id: string; action: 'approve' | 'deny' | 'cancel' }) => decideDiskRun(v.id, v.action),
    onSettled: done,
  })
}

export function useSaveDiskPolicy() {
  const done = useInvalidate()
  return useMutation({ mutationFn: (p: DiskGrowPolicy) => saveDiskPolicy(p), onSettled: done })
}
