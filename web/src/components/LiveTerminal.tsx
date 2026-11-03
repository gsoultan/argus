import { useCallback, useEffect, useRef, useState } from 'react'
import { FS } from '~/theme'
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
 * How long to keep trying: a little under the five minutes the control plane
 * keeps a shell whose browser has gone, so the last try can still find it.
 */
const RECONNECT_FOR_MS = 290_000

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
}

interface ServerMessage {
  type: 'output' | 'ready' | 'error' | 'closed' | 'gone' | 'moved'
  /** On ready: this is the shell that was already running, reattached. */
  resumed?: boolean
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
}: LiveTerminalProps) {
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

    // The session this terminal is showing, once the server has said which.
    // A dropped connection comes back to it rather than opening a new shell.
    let session: string | undefined
    // Set when the server has said the session is over, or is someone
    // else's now: nothing to come back to.
    let final = false
    let lostAt = 0
    let attempt = 0
    let retry: ReturnType<typeof setTimeout> | undefined
    let dialing = false

    const lose = (why: string) => {
      final = true
      setStatus('lost', why)
      term.writeln(`\r\n\x1b[2m${why}\x1b[0m`)
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
        lose('the connection was lost and could not be restored in time; the session was ended on the server')
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
    // Leaving the page is closing the terminal on purpose: say so, so the
    // shell is hung up now rather than kept for a return that is not coming.
    const onLeave = () => hangup(ws)
    window.addEventListener('online', onBack)
    document.addEventListener('visibilitychange', onBack)
    window.addEventListener('pagehide', onLeave)

    void connect()

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
            setStatus('live')
            if (msg.session && msg.session !== session) {
              session = msg.session
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
            lose(msg.data ? `could not reconnect: ${msg.data}` : 'could not reconnect: the session has ended')
            break
          case 'moved':
            lose(msg.data ?? 'this session was opened in another window')
            break
          case 'closed':
            final = true
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
      window.removeEventListener('pagehide', onLeave)
      observer.disconnect()
      onData.dispose()
      hangup(ws)
      term.dispose()
      termRef.current = null
      wsRef.current = null
    }
    // Reconnecting on every prop change would drop a live shell mid-command.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [endpoint, target, principal])

  return (
    <Stack gap={0} h="100%">
      <Group
        justify="space-between"
        px="sm"
        py={6}
        style={{ borderBottom: '1px solid var(--color-line)', background: 'var(--color-surface)' }}
      >
        <Group gap={8}>
          <StatusBadge state={state} />
          <Text size="xs" ff="monospace">
            <Text span c="teal.4" inherit>{principal}</Text>
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
      </Group>

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
  if (state === 'error') return <Badge size="xs" color="rose">error</Badge>
  return <Badge size="xs" color="slate">closed</Badge>
}
