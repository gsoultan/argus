import { useMutation, useQueryClient } from '@tanstack/react-query'
import { get, send } from '~/lib/live'

/*
 * Settings › Control plane › Backups. Kept out of live.ts and queries.ts,
 * which every route loads, so only the settings page pays for it.
 */

/** One backup under the backup directory; `complete` is false for a run that stopped. */
export interface ControlBackupRun {
  name: string
  complete: boolean
  at: string
  databaseBytes: number
  recordings: number
  chainEvents: number
  version?: string
}

/** Settings › Control plane › Backups, as GET /api/v1/platform/backups returns it. */
export interface ControlBackupsView {
  installed: boolean
  reason?: string
  canRunNow: boolean
  runNowReason?: string
  timerOn: boolean
  dir: string
  keep: number
  keepSetHere: boolean
  keepSetBy?: string
  last?: {
    startedAt: string
    finishedAt?: string
    ok: boolean
    error?: string
    run?: string
    pruned?: number
    keep?: number
  }
  running: boolean
  requested?: { by: string; at: string }
  late?: boolean
  runs: ControlBackupRun[]
  readFailure?: string
}

export async function controlBackups(): Promise<ControlBackupsView> {
  return get<ControlBackupsView>('/api/v1/platform/backups')
}

export async function setControlBackupKeep(keep: number): Promise<ControlBackupsView> {
  return send<ControlBackupsView>('PUT', '/api/v1/platform/backups/settings', { keep })
}

export async function runControlBackup(): Promise<ControlBackupsView> {
  return send<ControlBackupsView>('POST', '/api/v1/platform/backups/run')
}

export const controlBackupsKey = ['platform', 'backups'] as const

/** Sets how many of the control plane's own backups are kept. */
export function useSetControlBackupKeep() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (keep: number) => setControlBackupKeep(keep),
    onSuccess: (v) => qc.setQueryData(controlBackupsKey, v),
  })
}

/** Asks for a backup of the control plane now. */
export function useRunControlBackup() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => runControlBackup(),
    onSuccess: (v) => qc.setQueryData(controlBackupsKey, v),
    onSettled: () => void qc.invalidateQueries({ queryKey: controlBackupsKey }),
  })
}
