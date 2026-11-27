import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { get, send } from '~/lib/live'

/*
 * The organization's argus-secret servers (internal/control/secrets_servers.go
 * and secrets_tunnel.go). Admins and owners only. Kept in the console itself,
 * not in argus-secret's screens: adding, enrolling and fixing a server must
 * work before any server is up.
 */

export interface SecretsServer {
  id: string
  name: string
  mode: 'direct' | 'tunnel'
  url: string
  tlsPin: string
  folders: string[]
  sealKey: string
  version: string
  secretCount: number
  rulesSerial: number
  rulesPushedAt?: string
  logSeq: number
  logGap?: string
  logGapAt?: string
  lastContactAt?: string
  lastError?: string
  lastErrorAt?: string
  createdBy: string
  createdAt: string
  enrolled: boolean
  enrollExpiresAt?: string
  tunnelSeenAt?: string
  connected: boolean
}

export interface SecretsServersView {
  organization: string
  controlPublicKey: string
  servers: SecretsServer[]
}

export interface SecretsServerInput {
  name: string
  mode: 'direct' | 'tunnel'
  url: string
  tlsPin: string
  folders: string[]
}

/** A tunnel server just added, or given a new token: shown once. */
export type Enrolment = SecretsServer & { enrollToken: string }

const key = ['secrets-servers'] as const
const enc = encodeURIComponent

export const secretsServersQuery = () =>
  queryOptions({ queryKey: key, queryFn: () => get<SecretsServersView>('/api/v1/secrets/servers') })

function useServersMutation<A, R>(fn: (a: A) => Promise<R>) {
  const qc = useQueryClient()
  return useMutation({
    mutationFn: fn,
    onSettled: () => {
      void qc.invalidateQueries({ queryKey: key })
      void qc.invalidateQueries({ queryKey: ['secrets'] })
    },
  })
}

export const useAddSecretsServer = () =>
  useServersMutation((in_: SecretsServerInput) => send<SecretsServer | Enrolment>('POST', '/api/v1/secrets/servers', in_))
export const useChangeSecretsServer = () =>
  useServersMutation(({ id, input }: { id: string; input: SecretsServerInput }) =>
    send<SecretsServer>('PUT', `/api/v1/secrets/servers/${enc(id)}`, input))
export const useRemoveSecretsServer = () =>
  useServersMutation((id: string) => send<void>('DELETE', `/api/v1/secrets/servers/${enc(id)}`))
export const useSyncSecretsServer = () =>
  useServersMutation((id: string) => send<SecretsServer>('POST', `/api/v1/secrets/servers/${enc(id)}/sync`, {}))
export const useResumeSecretsLog = () =>
  useServersMutation(({ id, reason }: { id: string; reason: string }) =>
    send<SecretsServer>('POST', `/api/v1/secrets/servers/${enc(id)}/log/resume`, { reason }))
export const useNewEnrolToken = () =>
  useServersMutation((id: string) => send<Enrolment>('POST', `/api/v1/secrets/servers/${enc(id)}/enroll-token`, {}))

/** The command that enrolls a tunnel server, as the person runs it there. */
export function enrolCommand(token: string): string {
  const control = typeof window === 'undefined' ? 'https://argus.example.com' : window.location.origin
  return `sudo -u argus-secret argus-secret enroll --control ${control} --token ${token}\nsudo systemctl restart argus-secret`
}
