import { createContext, useCallback, useContext, useMemo, useRef, useState } from 'react'
import { useNavigate } from '@tanstack/react-router'
import { terminalTicket } from '~/lib/live'
import type { Asset } from '~/types/domain'

/**
 * The sessions this browser tab has open, kept above the routes.
 *
 * A session used to live inside the /connect page: leaving the page closed the
 * desktop, and there could only be one. Here each connection is an entry the
 * workspace (components/connect/Workspace) shows as a tab and keeps mounted
 * while you look at another tab or another page, so a desktop or a terminal
 * keeps running until it is ended on purpose.
 *
 * Opening still asks the control plane exactly as before: the ticket is
 * minted on Connect, and that answer is the permission. Nothing here opens a
 * session the control plane has not just allowed.
 */

/** What a Linux session shows: its terminal, its files, or both side by side. */
export type SessionView = 'terminal' | 'files' | 'split'

/** The ways into an asset this console can open today. */
export type Way = 'terminal' | 'files' | 'split' | 'desktop' | 'screen' | 'device'

export interface OpenSession {
  id: string
  target: string
  principal: string
  protocol: 'ssh' | 'rdp'
  way: Way
  /** Bumped by Connect again, to remount the views on a fresh ticket. */
  attempt: number
  openedAt: number
}

export interface OpenRequest {
  target: string
  principal: string
  way: Way
  /** The Windows password, for a Remote Desktop sign-in. Held until the desktop takes it. */
  password?: string
}

interface SessionsValue {
  sessions: OpenSession[]
  activeId: string | null
  /** Asks for a ticket and, if allowed, opens the session as a new tab. */
  open: (req: OpenRequest) => Promise<{ error?: string }>
  /** Starts the session again in its tab, on a new ticket. */
  reopen: (id: string, password?: string) => Promise<{ error?: string }>
  close: (id: string) => void
  focus: (id: string) => void
  /** The held ticket, once; then a fresh one each call. */
  ticketFor: (id: string) => Promise<string | null>
  /** The held password, once. */
  takePassword: (id: string) => string
  /** Which asset the Connect drawer is open for: '' for "pick one", null when closed. */
  drawerFor: string | null
  ask: (hostname?: string) => void
  closeDrawer: () => void
}

const Ctx = createContext<SessionsValue | null>(null)

export function protocolOf(way: Way): 'ssh' | 'rdp' {
  return way === 'desktop' || way === 'screen' || way === 'device' ? 'rdp' : 'ssh'
}

/** The ways an asset can be opened by, first the one to suggest. */
export function waysFor(asset: Asset): Way[] {
  if (asset.kind === 'android') return ['device']
  // A Windows server: its desktop first, and the agent's terminal and files
  // as well, which need no Windows password.
  if (asset.protocol === 'rdp') return [asset.desktopMode === 'screen' ? 'screen' : 'desktop', 'terminal', 'files', 'split']
  return ['terminal', 'files', 'split']
}

let seq = 0

export function SessionsProvider({ children }: { children: React.ReactNode }) {
  const navigate = useNavigate()
  const [sessions, setSessions] = useState<OpenSession[]>([])
  const [activeId, setActiveId] = useState<string | null>(null)
  const [drawerFor, setDrawerFor] = useState<string | null>(null)
  // Refs, not state: a ticket and a password are each read exactly once and
  // must not linger in anything React keeps or re-renders.
  const tickets = useRef(new Map<string, string>())
  const passwords = useRef(new Map<string, string>())
  const byId = useRef(new Map<string, OpenSession>())

  const open = useCallback(async (req: OpenRequest) => {
    const res = await terminalTicket(req.target, req.principal)
    if ('error' in res) return { error: res.error }
    const id = `s${++seq}`
    const s: OpenSession = {
      id, target: req.target, principal: req.principal, way: req.way,
      protocol: protocolOf(req.way), attempt: 0, openedAt: Date.now(),
    }
    tickets.current.set(id, res.ticket)
    if (req.password) passwords.current.set(id, req.password)
    byId.current.set(id, s)
    setSessions((list) => [...list, s])
    setActiveId(id)
    setDrawerFor(null)
    void navigate({ to: '/connect', search: {} })
    return {}
  }, [navigate])

  const reopen = useCallback(async (id: string, password?: string) => {
    const s = byId.current.get(id)
    if (!s) return { error: 'This session is no longer open.' }
    const res = await terminalTicket(s.target, s.principal)
    if ('error' in res) return { error: res.error }
    tickets.current.set(id, res.ticket)
    if (password) passwords.current.set(id, password)
    const next = { ...s, attempt: s.attempt + 1 }
    byId.current.set(id, next)
    setSessions((list) => list.map((x) => (x.id === id ? next : x)))
    return {}
  }, [])

  const close = useCallback((id: string) => {
    tickets.current.delete(id)
    passwords.current.delete(id)
    byId.current.delete(id)
    setSessions((list) => {
      const i = list.findIndex((x) => x.id === id)
      const rest = list.filter((x) => x.id !== id)
      setActiveId((cur) => (cur === id ? (rest[Math.min(i, rest.length - 1)]?.id ?? null) : cur))
      return rest
    })
  }, [])

  const ticketFor = useCallback(async (id: string) => {
    const held = tickets.current.get(id)
    if (held) {
      tickets.current.delete(id)
      return held
    }
    const s = byId.current.get(id)
    if (!s) return null
    const res = await terminalTicket(s.target, s.principal)
    return 'error' in res ? null : res.ticket
  }, [])

  const takePassword = useCallback((id: string) => {
    const p = passwords.current.get(id) ?? ''
    passwords.current.delete(id)
    return p
  }, [])

  const value = useMemo<SessionsValue>(() => ({
    sessions, activeId, open, reopen, close, focus: setActiveId, ticketFor, takePassword,
    drawerFor, ask: (h?: string) => setDrawerFor(h ?? ''), closeDrawer: () => setDrawerFor(null),
  }), [sessions, activeId, open, reopen, close, ticketFor, takePassword, drawerFor])

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

/**
 * Outside the shell (a component rendered on its own in a test) there are no
 * sessions and nothing to open: the stub says so rather than throwing.
 */
const NONE: SessionsValue = {
  sessions: [], activeId: null, drawerFor: null,
  open: async () => ({ error: 'Sessions open from the console.' }),
  reopen: async () => ({ error: 'Sessions open from the console.' }),
  close: () => undefined, focus: () => undefined, ticketFor: async () => null,
  takePassword: () => '', ask: () => undefined, closeDrawer: () => undefined,
}

export function useSessions(): SessionsValue {
  return useContext(Ctx) ?? NONE
}

/* ---------- remembering how you last connected ---------- */

const PREF_KEY = 'argus.connect.last'

export interface ConnectPref {
  way: Way
  principal: string
  /** Open this way straight from the asset's Connect button. */
  oneClick: boolean
}

function readPrefs(): Record<string, ConnectPref> {
  try {
    return JSON.parse(window.localStorage.getItem(PREF_KEY) ?? '{}') as Record<string, ConnectPref>
  } catch {
    return {}
  }
}

/** How this person last connected to the host, if the browser kept it. */
export function lastConnect(hostname: string): ConnectPref | undefined {
  return readPrefs()[hostname]
}

export function rememberConnect(hostname: string, pref: ConnectPref) {
  try {
    const all = readPrefs()
    all[hostname] = pref
    window.localStorage.setItem(PREF_KEY, JSON.stringify(all))
  } catch {
    // Storage refused: the drawer simply asks again next time.
  }
}
