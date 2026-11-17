import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { get, isConfigured, send } from '~/lib/live'

/*
 * Settings › Account security › Fleet recovery key: a key kept offline that
 * alone can move every agent to a new signing key. Argus holds the public
 * half; the private half is handed over once, when it is made.
 */

export interface RecoveryKeyView {
  set: boolean
  publicKey?: string
  fingerprint?: string
  createdAt?: string
  createdBy?: string
  holding: number
  total: number
  rekeyed?: string
}

export interface MadeRecoveryKey {
  fingerprint: string
  privateKey: string
  fileName: string
}

const key = ['platform', 'recovery-key'] as const

/** The answer, if it is one: anything else fails the query, not the page. */
export function checkedRecoveryKey(v: RecoveryKeyView): RecoveryKeyView {
  if (!v || typeof v !== 'object' || typeof v.set !== 'boolean') {
    throw new Error('argus-control did not say whether it has a recovery key')
  }
  return v
}

export function useRecoveryKey(enabled: boolean) {
  return useQuery({
    queryKey: key,
    queryFn: async () => checkedRecoveryKey(await get<RecoveryKeyView>('/api/v1/platform/recovery-key')),
    enabled: enabled && isConfigured(),
  })
}

export function useMakeRecoveryKey() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: () => send<MadeRecoveryKey>('POST', '/api/v1/platform/recovery-key'),
    onSettled: () => void qc.invalidateQueries({ queryKey: key }),
  })
}

/** "17 of 18", or what to say when none are counted yet. */
export function holdingLabel(v: Pick<RecoveryKeyView, 'holding' | 'total'>): string {
  if (v.total === 0) return 'No servers have checked in yet'
  return `${v.holding} of ${v.total}`
}

/** Hands the browser a text file to save. */
export function saveTextFile(name: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'text/plain' }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

export const REKEY_COMMAND = 'sudo argus-control rekey -recovery-key /media/usb/argus-recovery.key'
export const REPLACE_COMMAND = 'sudo argus-control recovery-key replace -old /media/usb/argus-recovery.key -out /media/usb/argus-recovery-new.key'
