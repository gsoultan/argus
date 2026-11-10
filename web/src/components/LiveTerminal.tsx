import { useCallback, useEffect, useRef, useState } from 'react'
import { FS, INK } from '~/theme'
import { Badge, Box, Group, Loader, Stack, Text } from '@mantine/core'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

/**
 * Live browser terminal.
 *
 * Connects to a WebSocket endpoint and renders a real shell: the control
 * plane's, which relays to the server's own agent through its tunnel, or a
 * gateway's. Both speak the same protocol, and both open the session only on
 * a ticket the control plane issued after its checks, so a session opened
 * here is not a lesser-controlled one.
 *
 * This exists for the cases a native client cannot cover: a contractor on a
 * locked-down laptop, an auditor who needs to look without being issued a key,
 * an on-call engineer on a borrowed machine.
 */

/**
 * `reconnecting`: the connection dropped and the terminal is getting back to
 * the same shell, which the server keeps running meanwhile. `lost`: it could
 * not, and the shell has ended or will; nobody ended it on purpose.
 */
export type TerminalState = 'connecting' | 'live' | 'reconnecting' | 'lost' | 'closed' | 'error'

/** How soon to try again after each failed reconnect. */
const RECONNECT_DELAYS_MS = [500, 1000, 2000, 4000, 8000, 15000]
/**
 * How long to keep trying. The server keeps a shell for five minutes, and a
 * shell still running a command for hours, so this tab keeps trying well past
 * the first; only the server saying the session is over stops it sooner.
 */
const RECONNECT_FOR_MS = 30 * 60_000

/**
 * The session this tab last had open on a host as an account, so a reload, a
 * crash of the tab, or going to another page and back picks the same shell up
 * instead of starting a new one. Per tab: another tab opens its own.
 */
const resumeKey = (target: string, principal: string) => `argus.terminal:${principal}@${target}`
function storedSession(target: string, principal: string): string | undefined {
  try {
    return sessionStorage.getItem(resumeKey(target, principal)) ?? undefined
  } catch {
    return undefined
  }
}
function storeSession(target: string, principal: string, id: string | undefined) {
  try {
    if (id) sessionStorage.setItem(resumeKey(target, principal), id)
    else sessionStorage.removeItem(resumeKey(target, principal))
  } catch {
    // Private mode or storage off: a reload then opens a new shell.
  }
}

/**
 * Closes the terminal on purpose: the server hangs the shell up now instead
 * of keeping it for a reconnect.
 */
function hangup(ws: WebSocket | null) {
  if (!ws) return
  if (ws.readyState === WebSocket.OPEN) {
    try {
      ws.send(JSON.stringify({ type: 'hangup' }))
    } catch {
      // Closing anyway.
    }
  }
  ws.close()
}

export interface LiveTerminalProps {
  /** The WebSocket endpoint as an http(s) URL; see terminalEndpoint. */
  endpoint: string
  /**
   * Fetches a ticket for this connection.
   *
   * A callback rather than a value because tickets are single-use: React
   * StrictMode mounts effects twice in development, and any reconnect needs a
   * fresh one. Passing a ticket in as a prop means the second attempt always
   * fails with "already used" — the property working exactly as intended,
   * against a component that assumed otherwise.
   */
  getTicket: () => Promise<string | null>
  target: string
  principal: string
  onStateChange?: (state: TerminalState, detail?: string) => void
  onSession?: (sessionId: string) => void
  /**
   * Filled with what ends the session on purpose, for a Disconnect button.
   * Anything else that takes the terminal away -- another page, a reload, a
   * closed tab -- leaves the shell running for this tab to come back to.
   */
  endRef?: { current: (() => void) | null }
  /**
   * The shell's current folder, whenever it says so: OSC 7 (file://host/path)
   * where the shell reports it, or the "user@host: ~/path" window title most
   * Linux bash prompts set. Best effort: a shell that reports neither is
   * simply never followed.
   */
  onCwd?: (path: string) => void
  /** Leaves out the status row, for the session workspace, which has its own. */
  bare?: boolean
}

interface ServerMessage {
  type: 'output' | 'ready' | 'error' | 'closed' | 'gone' | 'moved' | 'retry' | 'notice'
  /** On ready: this is the shell that was already running, reattached. */
  resumed?: boolean
  /** On notice: what it is about; see the control plane's terminalMessage. */
  kind?: string
  ends_in_s?: number
  data?: string
  session?: string
  chain_head?: string
  exit_code?: number
}

export function LiveTerminal({
  endpoint,
  getTicket,
  target,
  principal,
  onStateChange,
  onSession,
  endRef,
  onCwd,
  bare = false,
}: LiveTerminalProps) {
  // Read through a ref so a new callback does not reconnect the shell.
  const onCwdRef = useRef(onCwd)
  onCwdRef.current = onCwd
  const hostRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const fitRef = useRef<FitAddon | null>(null)
  const wsRef = useRef<WebSocket | null>(null)

  const [state, setState] = useState<TerminalState>('connecting')
  // Mirrors `state` for use inside socket callbacks, which close over the
  // value from the render that created them.
  const stateRef = useRef<TerminalState>('connecting')
  const [detail, setDetail] = useState<string>()
  const [sessionId, setSessionId] = useState<string>()
  const sessionIdRef = useRef<string | undefined>(undefined)

  const setStatus = useCallback(
    (s: TerminalState, d?: string) => {
      stateRef.current = s
      setState(s)
      setDetail(d)
      onStateChange?.(s, d)
    },
    [onStateChange],
  )

  useEffect(() => {
    if (!hostRef.current) return
    let disposed = false
    let ws: WebSocket | null = null

    const term = new Terminal({
      fontFamily: getComputedStyle(document.documentElement).getPropertyValue('--font-mono'),
      fontSize: 13,
      lineHeight: 1.35,
      cursorBlink: true,
      convertEol: false,
      // Matches the console's palette so a live session and a replayed one do
      // not look like two different products.
      theme: {
        background: '#05080c',
        foreground: '#e2e8f0',
        cursor: '#2dd4a7',
        selectionBackground: 'rgba(45,212,167,0.25)',
        black: '#0a0e14',
        red: '#f4576b',
        green: '#2dd4a7',
        yellow: '#f0b429',
        blue: '#60a5fa',
        magenta: '#c084fc',
        cyan: '#38bdf8',
        white: '#cbd5e1',
      },
    })
    const fit = new FitAddon()
    term.loadAddon(fit)
    term.open(hostRef.current)
    fit.fit()

    termRef.current = term
    fitRef.current = fit

    const cwd7 = term.parser.registerOscHandler(7, (data) => {
      const p = cwdFromOsc7(data)
      if (p) onCwdRef.current?.(p)
      return false
    })
    const cwdTitle = term.onTitleChange((title) => {
      const p = cwdFromTitle(title)
      if (p) onCwdRef.current?.(p)
    })

    // The session this terminal is showing, once the server has said which.
    // A dropped connection comes back to it rather than opening a new shell.
    // A session this tab left running here comes first.
    let session: string | undefined = storedSession(target, principal)
    // Set while picking up a stored session after a reload, so a session
    // that has ended since opens a fresh one instead of a dead end.
    let fromStore = !!session
    // Set when the server has said the session is over, or is someone
    // else's now: nothing to come back to.
    let final = false
    let lostAt = 0
    let attempt = 0
    let retry: ReturnType<typeof setTimeout> | undefined
    let dialing = false

    const lose = (why: string, forget = true) => {
      final = true
      if (forget) storeSession(target, principal, undefined)
      setStatus('lost', why)
      term.writeln(`\r\n\x1b[2m${why}\x1b[0m`)
    }
    // Picking up a stored session found it over: start a new one, said so.
    const startFresh = (why: string) => {
      fromStore = false
      session = undefined
      storeSession(target, principal, undefined)
      term.writeln(`\x1b[2m${why}; opening a new session\x1b[0m`)
      void connect()
    }

    const connect = async (resume?: string) => {
      dialing = true
      let ticket: string | null
      try {
        ticket = await getTicket()
      } catch {
        // Offline: the console itself is out of reach. Try again later
        // while reconnecting; a first connect has nothing to retry.
        dialing = false
        if (disposed) return
        if (resume) scheduleReconnect()
        else setStatus('error', 'could not reach the console')
        return
      }
      dialing = false
      if (disposed) return
      if (!ticket) {
        if (resume) lose('could not get authorisation to reconnect')
        else setStatus('error', 'could not obtain authorisation for this session')
        return
      }

      const url = new URL(endpoint, window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      url.searchParams.set('target', target)
      url.searchParams.set('principal', principal)
      // A browser cannot set headers on a WebSocket handshake, so the
      // credential travels in the query string. Safe only because a ticket is
      // single-use and expires in about a minute — a session cookie in this
      // position would be a serious leak.
      url.searchParams.set('ticket', ticket)
      if (resume) url.searchParams.set('resume', resume)

      ws = new WebSocket(url.toString())
      // Output arrives as binary frames from the control plane: written to
      // the terminal as bytes, so a character split across two frames is
      // decoded whole rather than replaced.
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws
      attach(ws)
    }

    /**
     * The connection went, but nobody ended the session: Wi-Fi changed, the
     * laptop slept, the phone put the tab away. The server keeps the shell
     * for a few minutes, so try to get back to it — promptly at first, then
     * less often, and at once when the page is visible or online again.
     */
    const reconnect = () => {
      if (disposed || final || !session || dialing) return
      if (retry) clearTimeout(retry)
      retry = undefined
      if (Date.now() - lostAt > RECONNECT_FOR_MS) {
        // Kept for this tab: Connect picks the shell up if it is still there.
        lose('the connection could not be restored for a long time; connect again to pick the session up if it is still running', false)
        return
      }
      void connect(session)
    }
    const scheduleReconnect = () => {
      if (stateRef.current !== 'reconnecting') {
        lostAt = Date.now()
        attempt = 0
        setStatus('reconnecting', 'connection lost')
        term.writeln('\r\n\x1b[2mconnection lost — reconnecting…\x1b[0m')
      }
      const delay = RECONNECT_DELAYS_MS[Math.min(attempt, RECONNECT_DELAYS_MS.length - 1)]
      attempt++
      retry = setTimeout(reconnect, delay)
    }
    const onBack = () => {
      if (stateRef.current === 'reconnecting' && document.visibilityState === 'visible') reconnect()
    }
    // Closing or reloading the tab while a session runs is asked about first;
    // a slip of the hand there used to hang the shell up mid-command. Even
    // when the person goes, the shell is held, and a reload picks it up.
    const onLeave = (ev: BeforeUnloadEvent) => {
      if (stateRef.current === 'live' || stateRef.current === 'reconnecting') {
        ev.preventDefault()
        ev.returnValue = ''
      }
    }
    window.addEventListener('online', onBack)
    document.addEventListener('visibilitychange', onBack)
    window.addEventListener('beforeunload', onLeave)
    if (endRef) {
      endRef.current = () => {
        final = true
        storeSession(target, principal, undefined)
        hangup(ws)
      }
    }

    void connect(session)

    function attach(sock: WebSocket) {
      const current = () => !disposed && wsRef.current === sock
      sock.onopen = () => {
        if (!session) term.writeln('\x1b[2mconnecting…\x1b[0m')
      }

      sock.onmessage = (ev) => {
        if (!current()) return
        if (ev.data instanceof ArrayBuffer) {
          term.write(new Uint8Array(ev.data))
          return
        }
        let msg: ServerMessage
        try {
          msg = JSON.parse(ev.data as string) as ServerMessage
        } catch {
          return
        }
        switch (msg.type) {
          case 'ready':
            // Back on the same shell: the server sends the recent screen next,
            // so start from a clean one rather than draw it twice.
            if (msg.resumed) term.reset()
            else term.clear()
            attempt = 0
            fromStore = false
            setStatus('live')
            if (msg.session) storeSession(target, principal, msg.session)
            if (msg.session && (msg.session !== session || !sessionIdRef.current)) {
              session = msg.session
              sessionIdRef.current = msg.session
              setSessionId(msg.session)
              onSession?.(msg.session)
            }
            // Send the real size now the shell exists, so the remote side is not
            // stuck on the 80x24 default — and, on a reattach, so a full-screen
            // program redraws for this window.
            queueMicrotask(() => {
              fit.fit()
              if (sock.readyState === WebSocket.OPEN) {
                sock.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }))
              }
            })
            break
          case 'output':
            if (msg.data) term.write(msg.data)
            break
          case 'error':
            final = true
            setStatus('error', msg.data)
            term.writeln(`\r\n\x1b[31margus: ${msg.data ?? 'connection refused'}\x1b[0m`)
            break
          case 'gone':
            if (fromStore) startFresh('the session this tab had open here has ended')
            else lose(msg.data ? `could not reconnect: ${msg.data}` : 'could not reconnect: the session has ended')
            break
          case 'retry':
            // Not an answer about the session: the control plane cannot
            // reach the agent yet (it has just restarted, or the network is
            // still out). The close that follows tries again.
            if (stateRef.current !== 'reconnecting') {
              lostAt = Date.now()
              attempt = 0
              setStatus('reconnecting', msg.data ?? 'waiting for the server')
            }
            break
          case 'notice':
            term.writeln(`\r\n\x1b[33margus: ${msg.data ?? ''}\x1b[0m`)
            if (msg.kind === 'agent-reconnecting' || msg.kind === 'control-restarting') {
              setStatus('reconnecting', msg.data)
            } else if (msg.kind === 'agent-back') {
              setStatus('live')
            } else if (msg.kind === 'restarting') {
              setStatus(stateRef.current, msg.data)
            }
            break
          case 'moved':
            lose(msg.data ?? 'this session was opened in another window')
            break
          case 'closed':
            final = true
            storeSession(target, principal, undefined)
            setStatus('closed', `exit ${msg.exit_code ?? 0}`)
            term.writeln(`\r\n\x1b[2msession ended (exit ${msg.exit_code ?? 0})\x1b[0m`)
            break
        }
      }

      // Before the session exists, a failed socket is a failed connect. After,
      // it is a dropped connection, and onclose tries to get back.
      sock.onerror = () => {
        if (current() && !session) setStatus('error', 'connection failed')
      }
      sock.onclose = () => {
        if (!current()) return
        if (session && !final) {
          scheduleReconnect()
          return
        }
        // A socket close after an error must not overwrite the error, or the
        // user is told the session "closed" when it was actually refused.
        if (stateRef.current === 'live' || stateRef.current === 'connecting') {
          setStatus('closed')
        }
      }
    }

    const onData = term.onData((data) => {
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'input', data }))
      }
    })

    // Keep the remote pty in step with the browser window, or full-screen tools
    // like top and vim draw into the wrong geometry.
    const observer = new ResizeObserver(() => {
      // Hidden behind the Files tab: keep the size it had rather than
      // shrinking the remote pty to nothing.
      const box = hostRef.current
      if (!box || box.clientWidth === 0 || box.clientHeight === 0) return
      try {
        fit.fit()
      } catch {
        return
      }
      if (ws && ws.readyState === WebSocket.OPEN) {
        ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }))
      }
    })
    observer.observe(hostRef.current)

    return () => {
      disposed = true
      if (retry) clearTimeout(retry)
      window.removeEventListener('online', onBack)
      document.removeEventListener('visibilitychange', onBack)
      window.removeEventListener('beforeunload', onLeave)
      if (endRef) endRef.current = null
      observer.disconnect()
      cwd7.dispose()
      cwdTitle.dispose()
      onData.dispose()
      // Not a hangup: going to another page leaves the shell running, held
      // on the server, for this tab to pick up; Disconnect ends it.
      ws?.close()
      term.dispose()
      termRef.current = null
      wsRef.current = null
    }
    // Reconnecting on every prop change would drop a live shell mid-command.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [endpoint, target, principal])

  return (
    <Stack gap={0} h="100%">
      {!bare && <Group
        justify="space-between"
        px="sm"
        py={6}
        style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--color-surface)' }}
      >
        <Group gap={8}>
          <StatusBadge state={state} />
          <Text size="xs" ff="monospace">
            <Text span c={INK.verified} inherit>{principal}</Text>
            <Text span c="dimmed" inherit>@</Text>
            {target}
          </Text>
          {detail && <Text size={FS.micro} c="dimmed">{detail}</Text>}
        </Group>
        {sessionId && (
          <Text size={FS.micro} c="dimmed" ff="monospace">
            recording {sessionId.slice(0, 12)}…
          </Text>
        )}
      </Group>}

      <Box style={{ flex: 1, minHeight: 0, background: '#05080c', padding: 8 }}>
        <div ref={hostRef} style={{ width: '100%', height: '100%' }} />
      </Box>
    </Stack>
  )
}

function StatusBadge({ state }: { state: TerminalState }) {
  if (state === 'connecting') {
    return (
      <Badge size="xs" color="slate" leftSection={<Loader size={8} color="slate" />}>
        connecting
      </Badge>
    )
  }
  if (state === 'live') {
    return (
      <Badge size="xs" color="sky" leftSection={<Box w={6} h={6} className="rounded-full animate-pulse" style={{ background: 'var(--color-live)' }} />}>
        live · recording
      </Badge>
    )
  }
  if (state === 'reconnecting') {
    return (
      <Badge size="xs" color="amber" leftSection={<Loader size={8} color="amber" />}>
        reconnecting · shell kept running
      </Badge>
    )
  }
  if (state === 'lost') return <Badge size="xs" color="amber">connection lost</Badge>
  if (state === 'error') return <Badge size="xs" color="rose">error</Badge>
  return <Badge size="xs" color="slate">closed</Badge>
}

/** The path in an OSC 7 report: file://host/path, percent-encoded. */
export function cwdFromOsc7(data: string): string | null {
  const m = /^file:\/\/[^/]*(\/.*)$/.exec(data.trim())
  if (!m) return null
  try {
    return decodeURIComponent(m[1]!)
  } catch {
    return null
  }
}

/** The path in a "user@host: path" title, as Debian and Ubuntu's bash set it. */
export function cwdFromTitle(title: string): string | null {
  const m = /^[\w.-]+@[\w.-]+:\s*((?:~|\/)[^\s]*)\s*$/.exec(title)
  return m ? m[1]! : null
}
