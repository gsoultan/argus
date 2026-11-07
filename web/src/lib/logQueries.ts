/*
 * Log cleanup. Live only: a sample of a server's log files shown as if it
 * were real would invite cleaning it, so against no control plane these
 * queries do not run and the views say they need one.
 */
import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import {
  assetLogs, decideLogRun, logPolicy, logRun, logRunFinished, saveLogPolicy, scanLogs, startLogRun,
} from '~/lib/logApi'
import { isConfigured, type LogCleanupPolicy, type LogPick } from '~/lib/live'

const viewKey = (assetId: string) => ['logs', 'asset', assetId] as const
const policyKey = ['logs', 'policy'] as const
const runKey = (id: string) => ['logs', 'run', id] as const

/** Fast while a scan or a cleanup is going, so its result shows as it lands. */
export const assetLogsQuery = (assetId: string) =>
  queryOptions({
    queryKey: viewKey(assetId),
    queryFn: () => assetLogs(assetId),
    enabled: isConfigured(),
    refetchInterval: (q) => {
      const v = q.state.data
      if (!v) return false
      const moving = v.scanning || v.scanAsked || v.runs.some((r) => !logRunFinished(r.state))
      return moving ? 3_000 : 60_000
    },
  })

export const logRunQuery = (id: string, enabled = true) =>
  queryOptions({
    queryKey: runKey(id),
    queryFn: () => logRun(id),
    enabled: isConfigured() && enabled && id !== '',
    refetchInterval: (q) => (q.state.data && !logRunFinished(q.state.data.state) ? 3_000 : false),
  })

export const logPolicyQuery = () =>
  queryOptions({ queryKey: policyKey, queryFn: logPolicy, enabled: isConfigured() })

function useInvalidate() {
  const qc = useQueryClient()
  return () => void qc.invalidateQueries({ queryKey: ['logs'] })
}

export function useScanLogs(assetId: string) {
  const done = useInvalidate()
  return useMutation({ mutationFn: () => scanLogs(assetId), onSettled: done })
}

export function useStartLogRun(assetId: string) {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (body: LogPick & { confirm: string }) => startLogRun(assetId, body),
    onSettled: done,
  })
}

export function useDecideLogRun() {
  const done = useInvalidate()
  return useMutation({
    mutationFn: (v: { id: string; action: 'approve' | 'deny' | 'cancel' }) => decideLogRun(v.id, v.action),
    onSettled: done,
  })
}

export function useSaveLogPolicy() {
  const done = useInvalidate()
  return useMutation({ mutationFn: (p: LogCleanupPolicy) => saveLogPolicy(p), onSettled: done })
}
