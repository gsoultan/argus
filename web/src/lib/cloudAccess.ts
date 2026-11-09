import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { get, isConfigured, send } from '~/lib/live'
import type { CloudAccount, CloudEnvironment } from '~/lib/cloud'

/*
 * Just-in-time console access, kept out of lib/cloud so the pages that read
 * cloud inventory do not carry it.
 */

export type ConsoleLevel = 'read' | 'operate'
export type CloudAccessState = 'awaiting_approval' | 'active' | 'expired' | 'declined' | 'cancelled' | 'ended'

/** One grant to an account's AWS console and command line. */
export interface CloudAccessGrant {
  id: string
  accountId: string
  accountName: string
  cloudAccountId: string
  level: ConsoleLevel
  environment: CloudEnvironment
  minutes: number
  reason: string
  state: CloudAccessState
  requestedBy: string
  requestedAt: string
  decidedBy: string | null
  decidedAt: string | null
  startsAt: string | null
  endsAt: string | null
  endedBy: string | null
  sessions: number
  lastSessionAt: string | null
}

export interface ConsoleLink { url: string; expires: string }
export interface CloudKeys { accessKeyId: string; secretAccessKey: string; sessionToken: string; expires: string; region: string }

const accessKey = ['cloud', 'access'] as const

export const cloudAccessQuery = (opts: { waiting?: boolean; enabled?: boolean } = {}) =>
  queryOptions({
    queryKey: [...accessKey, opts.waiting ? 'waiting' : 'all'],
    queryFn: () => get<{ grants: CloudAccessGrant[]; minutes: number[] }>(`/api/v1/cloud/access${opts.waiting ? '?waiting=1' : ''}`),
    enabled: isConfigured() && opts.enabled !== false,
    refetchInterval: 30_000,
  })

export function useAskCloudAccess() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: (in_: { accountId: string; level: ConsoleLevel; minutes: number; reason: string }) =>
      send<CloudAccessGrant>('POST', '/api/v1/cloud/access', in_),
    onSettled: () => void qc.invalidateQueries({ queryKey: accessKey }),
  })
}

export function useCloudAccessVerb() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, verb }: { id: string; verb: 'approve' | 'decline' | 'cancel' | 'end' }) =>
      send<CloudAccessGrant>('POST', `/api/v1/cloud/access/${encodeURIComponent(id)}/${verb}`),
    onSettled: () => void qc.invalidateQueries({ queryKey: accessKey }),
  })
}

/** Opens a session under a grant: a console sign-in link, or command-line keys. */
export function useCloudSession() {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: ({ id, keys, region }: { id: string; keys: boolean; region?: string }) =>
      send<ConsoleLink | CloudKeys>('POST', `/api/v1/cloud/access/${encodeURIComponent(id)}/${keys ? 'credentials' : 'console'}`, { region }),
    onSettled: () => void qc.invalidateQueries({ queryKey: accessKey }),
  })
}

/** Whether a grant needs a second person: the control plane's rule. */
export function accessNeedsApproval(env: CloudEnvironment, level: ConsoleLevel): boolean {
  if (env === 'production') return true
  if (env === 'staging') return level === 'operate'
  return false
}

/** Whether an account's stack has the role a level needs. */
export function consoleRoleMissing(a: CloudAccount | undefined, level: ConsoleLevel): boolean {
  return Boolean(a?.manageMissing?.includes(level === 'operate' ? 'role:ArgusConsoleOperate' : 'role:ArgusConsoleRead'))
}

/** A shell block that sets the keys for the AWS CLI and SDKs. */
export function keysBlock(k: CloudKeys): string {
  return [
    `export AWS_ACCESS_KEY_ID='${k.accessKeyId}'`,
    `export AWS_SECRET_ACCESS_KEY='${k.secretAccessKey}'`,
    `export AWS_SESSION_TOKEN='${k.sessionToken}'`,
    ...(k.region ? [`export AWS_REGION='${k.region}'`] : []),
  ].join('\n')
}

export function minutesText(m: number): string {
  return m < 60 ? `${m} minutes` : m === 60 ? '1 hour' : `${m / 60} hours`
}
