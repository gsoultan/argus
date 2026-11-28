import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { get, send } from '@argus/shell'
import { b64urlDecode, b64urlEncode, oneTimeKey, openSealed, readAAD, sealTo, stdB64Decode, stdB64Encode, writeAAD } from './seal'

/*
 * The secrets routes of argus-control (internal/control/secrets_routes.go).
 * Every request about one secret is relayed, signed for the signed-in person,
 * to the argus-secret server that keeps its folder, which decides it.
 */

export interface SecretVersion {
  n: number
  createdAt: string
  createdBy: string
  generated?: boolean
  fields?: string[]
}

export interface Secret {
  name: string
  type: 'text' | 'kv' | 'file'
  description?: string
  owner?: string
  versions: SecretVersion[]
  labels: Record<string, number>
  createdAt: string
  createdBy: string
  updatedAt: string
  deletedAt?: string
  deletedBy?: string
  purgeAt?: string
  server: string
  serverId: string
}

export interface SecretsList {
  secrets: Secret[]
  unreachable: { server: string; error: string }[]
}

export interface Subject { kind: string; id: string }

export interface SecretRule {
  id: string
  path: string
  subjects: Subject[]
  can: string[]
  needsApproval: boolean
  note: string
  createdBy: string
  createdAt: string
}

export interface SecretApp {
  id: string
  name: string
  tokenTail: string
  expiresAt: string
  revokedAt?: string
  revokedBy?: string
  createdBy: string
  createdAt: string
}

export interface ReadRequest {
  id: string
  requester: string
  name: string
  reason: string
  minutes: number
  state: 'pending' | 'approved' | 'denied' | 'cancelled'
  decidedBy?: string
  decidedAt?: string
  decisionNote?: string
  expiresAt?: string
  createdAt: string
  approveOwn?: boolean
}

const enc = encodeURIComponent
const KEY = ['secrets'] as const

export const secretsQuery = (prefix: string, deleted: boolean) => queryOptions({
  queryKey: [...KEY, 'list', prefix, deleted],
  queryFn: () => get<SecretsList>(`/api/v1/secrets?${new URLSearchParams({ ...(prefix ? { prefix } : {}), ...(deleted ? { deleted: '1' } : {}) })}`),
})
export const rulesQuery = () => queryOptions({ queryKey: [...KEY, 'rules'], queryFn: () => get<SecretRule[]>('/api/v1/secrets/rules') })
export const appsQuery = () => queryOptions({ queryKey: [...KEY, 'apps'], queryFn: () => get<SecretApp[]>('/api/v1/secrets/apps') })
export const requestsQuery = () => queryOptions({ queryKey: [...KEY, 'requests'], queryFn: () => get<ReadRequest[]>('/api/v1/secrets/requests') })

function useSecretsMutation<A, R>(fn: (a: A) => Promise<R>) {
  const qc = useQueryClient()
  return useMutation({ mutationFn: fn, onSettled: () => void qc.invalidateQueries({ queryKey: KEY }) })
}

/* ── Reading and writing values ────────────────────────────────────────── */

export interface Revealed { name: string; version: number; field: string; value: string }

/** The reason a read was refused because its rule wants an approval first. */
export const NEEDS_APPROVAL = 'needs an approval'

/**
 * Reads one value. It is sealed by argus-secret to a key made for this read,
 * which never leaves this tab, and opened here.
 */
export async function reveal(name: string, field: string, reason: string, version?: number): Promise<Revealed> {
  const k = await oneTimeKey()
  const r = await send<{ name: string; version: number; field?: string; sealed: string }>('POST', '/api/v1/secrets/read', {
    name, ...(field ? { field } : {}), ...(version ? { version } : {}), recipient: b64urlEncode(k.publicKey), reason,
  })
  if (r.name !== name || (r.field ?? '') !== field) throw new Error('argus-secret answered for another secret')
  const plain = await openSealed(k, stdB64Decode(r.sealed), readAAD(r.name, r.version, r.field ?? ''))
  return { name: r.name, version: r.version, field: r.field ?? '', value: new TextDecoder().decode(plain) }
}

export interface SecretInput {
  name: string
  type: 'text' | 'kv'
  description: string
  /** The value, or null to have argus-secret generate one. */
  value: string | null
  generateLength?: number
  reason: string
}

/** Writes a new version, sealed here to the server's own key. */
async function putSecret(in_: SecretInput) {
  const body: Record<string, unknown> = { name: in_.name, type: in_.type, description: in_.description, reason: in_.reason }
  if (in_.value === null) {
    body.generate = { length: in_.generateLength ?? 32 }
  } else {
    const { sealKey } = await get<{ server: string; sealKey: string }>(`/api/v1/secrets/seal-key?name=${enc(in_.name)}`)
    const sealed = await sealTo(b64urlDecode(sealKey), new TextEncoder().encode(in_.value), writeAAD(in_.name))
    body.sealedValue = stdB64Encode(sealed)
  }
  return send<Secret>('PUT', '/api/v1/secrets/item', body)
}

export const usePutSecret = () => useSecretsMutation(putSecret)
export const useDeleteSecret = () =>
  useSecretsMutation((a: { name: string; reason: string }) => send<Secret>('POST', '/api/v1/secrets/delete', a))
export const useRestoreSecret = () =>
  useSecretsMutation((a: { name: string }) => send<Secret>('POST', '/api/v1/secrets/restore', a))

/* ── Requests ──────────────────────────────────────────────────────────── */

export const useAskToRead = () =>
  useSecretsMutation((a: { name: string; reason: string; minutes: number }) => send<ReadRequest>('POST', '/api/v1/secrets/requests', a))
export const useDecide = () =>
  useSecretsMutation(({ id, ...body }: { id: string; approve: boolean; note: string; confirmSelf?: string }) =>
    send<ReadRequest>('POST', `/api/v1/secrets/requests/${enc(id)}/decide`, body))
export const useCancelRequest = () =>
  useSecretsMutation((id: string) => send<ReadRequest>('POST', `/api/v1/secrets/requests/${enc(id)}/cancel`, {}))

/* ── Rules and app tokens ──────────────────────────────────────────────── */

export type RuleInput = Omit<SecretRule, 'id' | 'createdBy' | 'createdAt'>

export const useSaveRule = () =>
  useSecretsMutation(({ id, rule }: { id?: string; rule: RuleInput }) =>
    id ? send<SecretRule>('PUT', `/api/v1/secrets/rules/${enc(id)}`, rule) : send<SecretRule>('POST', '/api/v1/secrets/rules', rule))
export const useRemoveRule = () => useSecretsMutation((id: string) => send<void>('DELETE', `/api/v1/secrets/rules/${enc(id)}`))

export const useMakeApp = () =>
  useSecretsMutation((a: { name: string; days: number }) => send<SecretApp & { token: string }>('POST', '/api/v1/secrets/apps', a))
export const useRevokeApp = () => useSecretsMutation((id: string) => send<void>('DELETE', `/api/v1/secrets/apps/${enc(id)}`))

/* ── Words ─────────────────────────────────────────────────────────────── */

export const CAN_LABEL: Record<string, string> = {
  list: 'See names', read: 'Read', write: 'Write', rotate: 'Rotate', manage: 'Manage',
}

export const SUBJECT_LABEL: Record<string, string> = {
  server: 'Server', group: 'Server group', app: 'App token', person: 'Person', team: 'Team', role: 'Role',
}
