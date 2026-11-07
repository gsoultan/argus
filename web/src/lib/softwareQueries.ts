import { useCallback } from 'react'
import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { isConfigured } from '~/lib/live'
import { runFinished } from '~/lib/packageApi'
import {
  installedSoftware,
  previewSoftware,
  searchSoftware,
  softwareRuns,
  startSoftwareRun,
  undoSoftwareRun,
  type SoftwareItem,
} from '~/lib/softwareApi'

const installedKey = (assetId: string, all: boolean) => ['software', 'installed', assetId, all] as const
const runsKey = (assetId: string) => ['software', 'runs', assetId] as const

/** Asked of the agent while the person looks, so not refetched on a timer. */
export const installedSoftwareQuery = (assetId: string, all: boolean, enabled = true) =>
  queryOptions({
    queryKey: installedKey(assetId, all),
    queryFn: () => installedSoftware(assetId, all),
    enabled: isConfigured() && enabled,
    staleTime: 60_000,
    retry: false,
  })

export const searchSoftwareQuery = (assetId: string, q: string) =>
  queryOptions({
    queryKey: ['software', 'search', assetId, q] as const,
    queryFn: () => searchSoftware(assetId, q),
    enabled: isConfigured() && q.trim().length >= 2,
    staleTime: 5 * 60_000,
    retry: false,
  })

/** Fast while one is going, so its state shows as it changes. */
export const softwareRunsQuery = (assetId: string, enabled = true) =>
  queryOptions({
    queryKey: runsKey(assetId),
    queryFn: () => softwareRuns(assetId),
    enabled: isConfigured() && enabled,
    refetchInterval: (q) => (q.state.data?.runs.some((r) => !runFinished(r.state)) ? 3_000 : 60_000),
  })

export function usePreviewSoftware(assetId: string) {
  return useMutation({
    mutationFn: (body: { action: 'install' | 'remove'; items: SoftwareItem[]; purge?: boolean; startServices?: boolean }) =>
      previewSoftware(assetId, body),
  })
}

export function useUndoSoftwareRun() {
  return useMutation({ mutationFn: (id: string) => undoSoftwareRun(id) })
}

export function useStartSoftwareRun(assetId: string) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (previewId: string) => startSoftwareRun(assetId, previewId),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ['software'] })
      void qc.invalidateQueries({ queryKey: ['packages'] })
    },
  })
}

/** After a run ends, what is installed has changed. */
export function useRefreshSoftware() {
  const qc = useQueryClient()
  return useCallback(() => {
    void qc.invalidateQueries({ queryKey: ['software'] })
    void qc.invalidateQueries({ queryKey: ['packages'] })
  }, [qc])
}
