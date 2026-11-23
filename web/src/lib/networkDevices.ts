import { queryOptions, useMutation, useQueryClient } from '@tanstack/react-query'
import { get, send } from '~/lib/live'
import type { RoundOutcome, WebsiteOutage, WebsiteRound, WebsiteState, WebsiteUptime } from '~/lib/websites'

/*
 * Network devices: a switch, router, firewall, printer, UPS or access point,
 * read over SNMP by the agent of a server near it (internal/control/
 * network_devices.go). The second kind of Monitoring check: up, down,
 * uptime, outages, groups and who can see it work as for websites. They are
 * not Assets: nobody opens a session to one.
 *
 * Its credentials go in and never come back: hasSecret says some are saved,
 * and an empty field on an edit keeps them.
 */

export type SnmpVersion = 'v3' | 'v2c'

export interface WatchedInterface {
  index: number
  name: string
}

export interface DeviceInterface {
  index: number
  name: string
  alias?: string
  up: boolean
  adminUp: boolean
  speedMbps?: number
  inOctets: number
  outOctets: number
  inErrors: number
  outErrors: number
  /** Traffic since the answer before, in bits a second. */
  inBps?: number
  outBps?: number
  /** Errors counted since the answer before. */
  errorsUp?: number
  watched?: boolean
}

/** What the device last said about itself. */
export interface DeviceInfo {
  name: string
  description: string
  location?: string
  uptimeSec: number
  at: string
  restartedAt?: string
  interfaces: DeviceInterface[]
}

export interface NetworkDevice {
  id: string
  name: string
  host: string
  port: number
  version: SnmpVersion
  user: string
  authProto: string
  privProto: string
  watch: WatchedInterface[]
  hasSecret: boolean
  everySeconds: number
  timeoutSeconds: number
  checkFrom: string[]
  checkHosts: string[]
  paused: boolean
  groupId: string
  createdBy: string
  createdAt: string
  state: WebsiteState
  stateSince?: string
  failingRounds: number
  lastAt?: string
  lastMs?: number
  lastDetail: string
  lastOutcome: RoundOutcome | ''
  device?: DeviceInfo
  uptime: WebsiteUptime
  openOutage?: WebsiteOutage
  canOperate: boolean
  /** Shown to someone outside the fleet roles: how it is read is left out. */
  limited?: boolean
}

export interface NetworkDeviceDetail {
  device: NetworkDevice
  rounds: WebsiteRound[]
  outages: WebsiteOutage[]
}

export interface NetworkDeviceInput {
  name: string
  host: string
  port: number
  version: SnmpVersion
  community: string
  user: string
  authProto: string
  authPass: string
  privProto: string
  privPass: string
  watch: WatchedInterface[]
  everySeconds: number
  timeoutSeconds: number
  checkFrom: string[]
  paused: boolean
}

/** One read, as the Test button gets it back. */
export interface DeviceAttempt {
  ok: boolean
  fail?: string
  detail?: string
  ms: number
  from?: string
  snmp?: { name: string; description: string; location?: string; uptimeSec: number; interfaces: DeviceInterface[] }
}

/** What "Find it" gets back: the v2c community that answered, if any. */
export interface SnmpDiscovery {
  found: boolean
  community?: string
  snmp?: { name: string; description: string; location?: string; uptimeSec: number; interfaces: DeviceInterface[] }
  tried: number
  detail?: string
}

/** The two read-only defaults a lot of gear ships with, always tried. */
export const SNMP_DEFAULT_COMMUNITIES = ['public', 'private'] as const

export const AUTH_PROTOS = ['SHA', 'SHA224', 'SHA256', 'SHA384', 'SHA512', 'MD5'] as const
export const PRIV_PROTOS = ['AES', 'AES192', 'AES256', 'DES'] as const

const key = ['network-devices'] as const
const path = (id: string) => `/api/v1/network-devices/${encodeURIComponent(id)}`

export const networkDevicesQuery = () =>
  queryOptions({ queryKey: key, queryFn: () => get<NetworkDevice[]>('/api/v1/network-devices'), refetchInterval: 30_000 })

export const networkDeviceQuery = (id: string) =>
  queryOptions({ queryKey: [...key, id] as const, queryFn: () => get<NetworkDeviceDetail>(path(id)), refetchInterval: 30_000 })

function useDeviceMutation<A, R>(fn: (a: A) => Promise<R>) {
  const qc = useQueryClient()
  return useMutation({ mutationFn: fn, onSettled: () => void qc.invalidateQueries({ queryKey: key }) })
}

export const useAddNetworkDevice = () =>
  useDeviceMutation((in_: NetworkDeviceInput) => send<NetworkDevice>('POST', '/api/v1/network-devices', in_))

export const useChangeNetworkDevice = () =>
  useDeviceMutation(({ id, input }: { id: string; input: NetworkDeviceInput }) => send<NetworkDevice>('PUT', path(id), input))

export const useRemoveNetworkDevice = () =>
  useDeviceMutation((id: string) => send<void>('DELETE', path(id)))

export const useCheckDeviceNow = () =>
  useDeviceMutation((id: string) => send<void>('POST', `${path(id)}/check`))

export const usePauseDevice = () =>
  useDeviceMutation(({ id, paused }: { id: string; paused: boolean }) => send<void>('POST', `${path(id)}/pause`, { paused }))

export const testNetworkDevice = (in_: NetworkDeviceInput, id?: string) =>
  send<DeviceAttempt>('POST', `/api/v1/network-devices/test${id ? `?id=${encodeURIComponent(id)}` : ''}`, in_)

/**
 * Ask a server on the device's network to find its v2c community string by
 * trying the common defaults plus `extra`, read-only. The strings are never
 * echoed back; the reply names only the one that worked.
 */
export const discoverCommunity = (host: string, port: number, checkFrom: string[], extra: string[]) =>
  send<SnmpDiscovery>('POST', '/api/v1/network-devices/discover', { host, port, checkFrom, communities: extra })

/** "1.2 Gb/s", "340 kb/s", "0 b/s", or "—". */
export function formatBps(v: number | undefined): string {
  if (v == null) return '—'
  const units = ['b/s', 'kb/s', 'Mb/s', 'Gb/s', 'Tb/s']
  let i = 0
  let n = v
  while (n >= 1000 && i < units.length - 1) { n /= 1000; i++ }
  return `${n >= 100 || i === 0 ? Math.round(n) : n.toFixed(1)} ${units[i]}`
}

/** "1 Gb/s", "100 Mb/s", or "—". */
export function formatSpeed(mbps: number | undefined): string {
  if (!mbps) return '—'
  return mbps >= 1000 ? `${mbps % 1000 === 0 ? mbps / 1000 : (mbps / 1000).toFixed(1)} Gb/s` : `${mbps} Mb/s`
}

/** How long it has been up: "12 d 4 h", "3 h 5 min". */
export function formatUptimeSec(s: number): string {
  const mins = Math.floor(s / 60)
  if (mins < 1) return 'under a minute'
  if (mins < 60) return `${mins} min`
  const h = Math.floor(mins / 60)
  if (h < 24) return mins % 60 ? `${h} h ${mins % 60} min` : `${h} h`
  const d = Math.floor(h / 24)
  return h % 24 ? `${d} d ${h % 24} h` : `${d} d`
}

/** An interface by its name, with the alias someone gave it. */
export const interfaceLabel = (i: Pick<DeviceInterface, 'name' | 'alias' | 'index'>) =>
  `${i.name || `Interface ${i.index}`}${i.alias ? ` (${i.alias})` : ''}`

/** How the device is read, in a few words: "SNMPv3, SHA256 and AES" or "SNMP v2c". */
export function securityWords(d: Pick<NetworkDevice, 'version' | 'authProto' | 'privProto'>): string {
  if (d.version === 'v2c') return 'SNMP v2c, community string in the clear'
  if (!d.authProto) return 'SNMPv3 without authentication'
  if (!d.privProto) return `SNMPv3, ${d.authProto} authentication, not encrypted`
  return `SNMPv3, ${d.authProto} and ${d.privProto} encryption`
}
