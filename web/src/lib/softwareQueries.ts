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
  checkScript,
  createScript,
  removeScript,
  reviewScript,
  runScript,
  runAndroidApp,
  softwareFiles,
  uploadSoftwareFile,
  removeSoftwareFile,
  type AppAction,
  saveScriptVersion,
  script,
  scriptRules,
  scripts,
  type ScriptPlatform,
  type ScriptVersionInput,
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
    // A device's apps come with its facts, a little after a change ends.
    void qc.invalidateQueries({ predicate: (q) => q.queryKey[0] === 'asset' && q.queryKey[2] === 'android-apps' })
  }, [qc])
}

/* ── Install scripts ─────────────────────────────────────────────────────── */

export const scriptsQuery = (enabled = true) =>
  queryOptions({ queryKey: ['software', 'scripts'] as const, queryFn: scripts, enabled: isConfigured() && enabled })

export const scriptQuery = (id: string) =>
  queryOptions({ queryKey: ['software', 'scripts', id] as const, queryFn: () => script(id), enabled: isConfigured() && id !== '' })

export const scriptRulesQuery = (platform: ScriptPlatform) =>
  queryOptions({ queryKey: ['software', 'script-rules', platform] as const, queryFn: () => scriptRules(platform),
    enabled: isConfigured(), staleTime: Infinity })

function useScriptMutation<V, R>(fn: (v: V) => Promise<R>) {
  const qc = useQueryClient()
  return useMutation({ mutationFn: (v: V) => fn(v), onSettled: () => void qc.invalidateQueries({ queryKey: ['software'] }) })
}

export const useCreateScript = () => useScriptMutation(createScript)
export const useSaveScriptVersion = () =>
  useScriptMutation((v: { id: string; body: ScriptVersionInput }) => saveScriptVersion(v.id, v.body))
export const useRemoveScript = () => useScriptMutation((id: string) => removeScript(id))
export const useReviewScript = () =>
  useScriptMutation((v: { versionId: string; accepted: { key: string; reason: string }[]; confirmSelf?: string }) =>
    reviewScript(v.versionId, { accepted: v.accepted, confirmSelf: v.confirmSelf }))
export const useRunScript = (assetId: string) => {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { versionId: string; action: 'install' | 'uninstall'; params: Record<string, string> }) => runScript(assetId, v),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ['software'] })
      void qc.invalidateQueries({ queryKey: ['packages'] })
    },
  })
}
export const useCheckScript = () =>
  useMutation({ mutationFn: (v: Parameters<typeof checkScript>[0]) => checkScript(v) })

/* ── Library files and apps on devices ───────────────────────────────────── */

export const softwareFilesQuery = (platform?: 'android' | 'windows') =>
  queryOptions({ queryKey: ['software', 'files', platform ?? 'all'] as const, queryFn: () => softwareFiles(platform) })

export const useUploadSoftwareFile = () =>
  useScriptMutation((v: { file: File; note: string }) => uploadSoftwareFile(v.file, v.note))
export const useRemoveSoftwareFile = () => useScriptMutation((id: string) => removeSoftwareFile(id))

export const useAndroidAppRun = (assetId: string) => {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (v: { action: AppAction; fileId?: string; package?: string }) => runAndroidApp(assetId, v),
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: ['software'] })
      void qc.invalidateQueries({ queryKey: ['asset', assetId] })
    },
  })
}
