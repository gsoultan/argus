import { useCallback, useEffect, useRef, useState } from 'react'
import { FS } from '~/theme'
import { Badge, Box, Group, Loader, Stack, Text } from '@mantine/core'
import { Terminal } from '@xterm/xterm'
import { FitAddon } from '@xterm/addon-fit'
import '@xterm/xterm/css/xterm.css'

/**
 * Live browser terminal.
 *
 * Connects to the gateway's WebSocket endpoint and renders a real shell. The
 * session is brokered through exactly the same path as ssh(1) — same principal
 * check, same host-key pin, same credential injection, same recording — so a
 * session opened here is not a lesser-controlled one.
 *
 * This exists for the cases a native client cannot cover: a contractor on a
 * locked-down laptop, an auditor who needs to look without being issued a key,
 * an on-call engineer on a borrowed machine.
 */

export type TerminalState = 'connecting' | 'live' | 'closed' | 'error'

export interface LiveTerminalProps {
  gatewayUrl: string
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
  type: 'output' | 'ready' | 'error' | 'closed'
  data?: string
  session?: string
  chain_head?: string
  exit_code?: number
}

export function LiveTerminal({
  gatewayUrl,
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

    void (async () => {
      const ticket = await getTicket()
      if (disposed) return
      if (!ticket) {
        setStatus('error', 'could not obtain authorisation for this session')
        return
      }

      const url = new URL('/ws/session', gatewayUrl)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      url.searchParams.set('target', target)
      url.searchParams.set('principal', principal)
      // A browser cannot set headers on a WebSocket handshake, so the
      // credential travels in the query string. Safe only because a ticket is
      // single-use and expires in about a minute — a session cookie in this
      // position would be a serious leak.
      url.searchParams.set('ticket', ticket)

      ws = new WebSocket(url.toString())
      wsRef.current = ws
      attach(ws)
    })()

    function attach(ws: WebSocket) {
    ws.onopen = () => term.writeln('\x1b[2mconnecting…\x1b[0m')

    ws.onmessage = (ev) => {
      let msg: ServerMessage
      try {
        msg = JSON.parse(ev.data as string) as ServerMessage
      } catch {
        return
      }
      switch (msg.type) {
        case 'ready':
          term.clear()
          setStatus('live')
          if (msg.session) {
            setSessionId(msg.session)
            onSession?.(msg.session)
          }
          // Send the real size now the shell exists, so the remote side is not
          // stuck on the 80x24 default.
          queueMicrotask(() => {
            fit.fit()
            ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }))
          })
          break
        case 'output':
          if (msg.data) term.write(msg.data)
          break
        case 'error':
          setStatus('error', msg.data)
          term.writeln(`\r\n\x1b[31margus: ${msg.data ?? 'connection refused'}\x1b[0m`)
          break
        case 'closed':
          setStatus('closed', `exit ${msg.exit_code ?? 0}`)
          term.writeln(`\r\n\x1b[2msession ended (exit ${msg.exit_code ?? 0})\x1b[0m`)
          break
      }
    }

    ws.onerror = () => setStatus('error', 'connection failed')
    // A socket close after an error must not overwrite the error, or the user
    // is told the session "closed" when it was actually refused.
    ws.onclose = () => {
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
      observer.disconnect()
      onData.dispose()
      ws?.close()
      term.dispose()
      termRef.current = null
      wsRef.current = null
    }
    // Reconnecting on every prop change would drop a live shell mid-command.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gatewayUrl, target, principal])

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
      <Badge size="xs" color="sky" leftSection={<Box w={6} h={6} className="rounded-full bg-sky-400 animate-pulse" />}>
        live · recording
      </Badge>
    )
  }
  if (state === 'error') return <Badge size="xs" color="rose">error</Badge>
  return <Badge size="xs" color="slate">closed</Badge>
}
