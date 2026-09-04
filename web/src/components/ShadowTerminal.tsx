import { useEffect, useRef, useState } from 'react'
import { Badge, Box, Group, Loader, Stack, Text } from '@mantine/core'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'

export type ShadowState = 'connecting' | 'watching' | 'ended' | 'error'

export interface ShadowTerminalProps {
  gatewayUrl: string
  sessionId: string
  /** Fetches a ticket. A callback because tickets are single-use. */
  getTicket: () => Promise<string | null>
  onStateChange?: (state: ShadowState, detail?: string) => void
}

interface ServerMessage {
  type: 'output' | 'resize' | 'ready' | 'error' | 'closed'
  data?: string
  session?: string
}

/**
 * A read-only view of a session someone else is running.
 *
 * There is no onData handler and nothing is ever sent on the socket. Read-only
 * is a property of this component having no code path to the target's stdin,
 * not a flag the gateway is trusted to honour — though the gateway refuses to
 * read from a shadow socket as well, so neither side relies on the other.
 */
export function ShadowTerminal({
  gatewayUrl,
  sessionId,
  getTicket,
  onStateChange,
}: ShadowTerminalProps) {
  const hostRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)

  const [state, setState] = useState<ShadowState>('connecting')
  const stateRef = useRef<ShadowState>('connecting')
  const [detail, setDetail] = useState<string>()
  // The subject's terminal size, which is what the output is formatted for.
  const [dims, setDims] = useState({ cols: 80, rows: 24 })

  useEffect(() => {
    if (!hostRef.current) return
    let disposed = false

    const setStatus = (s: ShadowState, d?: string) => {
      stateRef.current = s
      setState(s)
      setDetail(d)
      onStateChange?.(s, d)
    }

    const term = new Terminal({
      fontFamily: getComputedStyle(document.documentElement).getPropertyValue('--font-mono'),
      fontSize: 12,
      lineHeight: 1.35,
      // No cursor blink: nothing here is waiting for this viewer to type, and a
      // blinking cursor invites them to try.
      cursorBlink: false,
      disableStdin: true,
      convertEol: false,
      theme: {
        background: '#05080c',
        foreground: '#e2e8f0',
        cursor: '#f0b429',
        selectionBackground: 'rgba(240,180,41,0.25)',
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
    term.open(hostRef.current)
    termRef.current = term

    // Deliberately no FitAddon. A shadower's window is a different size from
    // the subject's, and resizing to fit the viewer would rewrap output that
    // was drawn for the subject's geometry — cursor-positioned UIs like top or
    // vim would render as garbage. The terminal matches the subject instead,
    // and the container scrolls.
    term.resize(dims.cols, dims.rows)

    ;(async () => {
      const ticket = await getTicket()
      if (disposed) return
      if (!ticket) {
        setStatus('error', 'not authorised to watch this session')
        term.writeln('\x1b[31margus: not authorised to watch this session\x1b[0m')
        return
      }

      const url = new URL('/ws/shadow', gatewayUrl)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      url.searchParams.set('session', sessionId)
      url.searchParams.set('ticket', ticket)

      const ws = new WebSocket(url.toString())
      wsRef.current = ws

      ws.onmessage = (ev) => {
        let msg: ServerMessage
        try {
          msg = JSON.parse(ev.data as string) as ServerMessage
        } catch {
          return
        }
        switch (msg.type) {
          case 'ready':
            setStatus('watching', msg.data)
            break
          case 'output':
            if (msg.data) term.write(msg.data)
            break
          case 'resize': {
            // The subject resized their window. Follow it, or everything drawn
            // afterwards is positioned against the wrong geometry.
            const parts = (msg.data ?? '').split('x')
            const c = Number(parts[0])
            const r = Number(parts[1])
            if (Number.isFinite(c) && Number.isFinite(r) && c > 0 && r > 0) {
              term.resize(c, r)
              setDims({ cols: c, rows: r })
            }
            break
          }
          case 'error':
            setStatus('error', msg.data)
            term.writeln(`\r\n\x1b[31margus: ${msg.data ?? 'refused'}\x1b[0m`)
            break
          case 'closed':
            setStatus('ended', msg.data)
            term.writeln(`\r\n\x1b[2m── ${msg.data ?? 'session ended'} ──\x1b[0m`)
            break
        }
      }

      ws.onerror = () => setStatus('error', 'connection failed')
      ws.onclose = () => {
        if (stateRef.current === 'watching' || stateRef.current === 'connecting') {
          setStatus('ended', 'disconnected')
        }
      }
    })()

    return () => {
      disposed = true
      wsRef.current?.close()
      term.dispose()
      termRef.current = null
    }
    // dims is state the effect sets, not an input to it; including it would
    // tear down the socket on every resize the subject makes.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gatewayUrl, sessionId])

  return (
    <Stack gap="xs">
      <Group justify="space-between">
        <Group gap="xs">
          {state === 'connecting' && <Loader size="xs" />}
          <Badge
            color={
              state === 'watching' ? 'amber' : state === 'error' ? 'rose' : 'slate'
            }
            variant="light"
          >
            {state === 'watching' ? 'read only · live' : state}
          </Badge>
          <Text size="xs" c="dimmed">
            {dims.cols}×{dims.rows}
          </Text>
        </Group>
        {detail && (
          <Text size="xs" c="dimmed">
            {detail}
          </Text>
        )}
      </Group>
      <Box
        ref={hostRef}
        style={{
          background: '#05080c',
          border: '1px solid rgba(240,180,41,0.35)',
          borderRadius: 8,
          padding: 8,
          overflow: 'auto',
        }}
      />
    </Stack>
  )
}
