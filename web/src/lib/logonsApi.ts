import { get, send } from '~/lib/live'

/*
 * Who is signed in on a host, asked of its agent when the Connect page opens,
 * and signing out a disconnected Windows session. Here rather than in live.ts,
 * which every page loads.
 */

export interface Logon {
  /** Windows' session id; 0 on Linux. */
  session?: number
  account: string
  state: 'active' | 'disconnected'
  /** The session on the host's own screen. */
  console?: boolean
  remote?: boolean
  /** The terminal a Linux login is on. */
  line?: string
  /** The client's name or address. */
  from?: string
  logonAt?: string
  idleSeconds?: number
}

export interface LogonsView {
  state: 'ok' | 'offline' | 'unsupported' | 'failed'
  error?: string
  platform?: string
  edition?: 'workstation' | 'server'
  /** How many Remote Desktop sessions Windows takes at once; 0 is no fixed limit. */
  remoteLimit?: number
  logons: Logon[]
  canSignOut: boolean
  signedOut?: number
}

export async function assetLogons(assetId: string): Promise<LogonsView> {
  return get(`/api/v1/assets/${encodeURIComponent(assetId)}/logons`)
}

export async function signOutLogon(assetId: string, session: number, account: string): Promise<LogonsView> {
  return send('POST', `/api/v1/assets/${encodeURIComponent(assetId)}/logons/signout`, { session, account })
}

export const logonsKey = (assetId: string) => ['assets', assetId, 'logons'] as const

/** Remote Desktop sessions in use: the console does not take a slot. */
export function remoteInUse(v: LogonsView): number {
  return v.logons.filter((l) => l.remote).length
}

/** Whether a new Remote Desktop session would find no free slot. */
export function slotsFull(v: LogonsView): boolean {
  return v.platform === 'windows' && !!v.remoteLimit && remoteInUse(v) >= v.remoteLimit
}

/**
 * What connecting as `principal` will do on this host, in one sentence, or
 * undefined when nothing is worth saying. Account names on Windows are
 * DOMAIN\name; the picked principal is a bare local name.
 */
export function connectOutcome(v: LogonsView, principal: string): string | undefined {
  if (v.platform !== 'windows') return undefined
  const same = (l: Logon) => {
    const name = l.account.split('\\').pop() ?? l.account
    return name.toLowerCase() === principal.toLowerCase()
  }
  const mine = v.logons.find(same)
  if (mine?.console && mine.state === 'active') {
    return `${principal} is signed in on the ${v.edition === 'workstation' ? 'PC' : 'server'}'s own screen. Opening it moves that session here and locks the screen.`
  }
  if (mine?.state === 'disconnected') {
    return `${principal} has a disconnected session. Opening goes back into it, as it was left.`
  }
  if (mine) return `${principal} is already signed in from ${mine.from || 'elsewhere'}. Opening takes that session over.`
  if (v.edition === 'workstation' && v.logons.some((l) => l.state === 'active')) {
    return 'Someone is using this PC. Windows takes one person at a time, so it will ask them to let you in, and sign them out of the screen if they do.'
  }
  if (slotsFull(v)) {
    return 'Every Remote Desktop session this server allows is taken. Windows would ask someone to give theirs up; an admin can sign out a disconnected one below instead.'
  }
  return undefined
}
