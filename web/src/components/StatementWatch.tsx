import { useEffect, useRef, useState } from 'react'
import { Badge, Box, Group, Loader, Stack, Text } from '@mantine/core'
import { PostgresReplay } from '~/components/PostgresReplay'
import type { ShadowState } from '~/components/ShadowTerminal'
import { FS } from '~/theme'

/*
A read-only view of a database session someone else is running: its
statements (or Redis and RabbitMQ commands) as they are recorded, each with
what the engine answered.

The control plane sends recording lines, the same lines the replay reads, so
what is watched and what is replayed afterwards cannot disagree. Results are
not shown, because they are never recorded. A viewer arriving late gets the
session's recent lines first.

Like ShadowTerminal, nothing is ever sent on the socket.
*/

export interface StatementWatchProps {
  /** The watch endpoint as an http(s) URL; see watchEndpoint. */
  endpoint: string
  sessionId: string
  /** Fetches a ticket. A callback because tickets are single-use. */
  getTicket: () => Promise<string | null>
  onStateChange?: (state: ShadowState, detail?: string) => void
}

interface ServerMessage {
  type: 'ready' | 'record' | 'error' | 'closed' | 'resize' | 'output'
  data?: string
}

/** The most lines held, so a long session watched for hours stays bounded. */
export const WATCH_MAX_LINES = 5_000

export function StatementWatch({ endpoint, sessionId, getTicket, onStateChange }: StatementWatchProps) {
  const [state, setState] = useState<ShadowState>('connecting')
  const stateRef = useRef<ShadowState>('connecting')
  const [detail, setDetail] = useState<string>()
  const [lines, setLines] = useState<string[]>([])
  const [dropped, setDropped] = useState(false)
  const bottomRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    let disposed = false
    let ws: WebSocket | null = null
    const setStatus = (s: ShadowState, d?: string) => {
      stateRef.current = s
      setState(s)
      setDetail(d)
      onStateChange?.(s, d)
    }
    ;(async () => {
      const ticket = await getTicket()
      if (disposed) return
      if (!ticket) {
        setStatus('error', 'not authorised to watch this session')
        return
      }
      const url = new URL(endpoint, window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      url.searchParams.set('ticket', ticket)
      ws = new WebSocket(url.toString())
      ws.onmessage = (ev) => {
        // A database session sends nothing binary; anything that is would be
        // terminal output, which this view has no business drawing.
        if (typeof ev.data !== 'string') return
        let msg: ServerMessage
        try {
          msg = JSON.parse(ev.data) as ServerMessage
        } catch {
          return
        }
        switch (msg.type) {
          case 'ready':
            setStatus('watching')
            break
          case 'record': {
            const line = msg.data
            if (!line) break
            setLines((prev) => {
              const next = [...prev, line]
              if (next.length > WATCH_MAX_LINES) {
                setDropped(true)
                return next.slice(next.length - WATCH_MAX_LINES)
              }
              return next
            })
            break
          }
          case 'error':
            setStatus('error', msg.data ?? 'refused')
            break
          case 'closed':
            setStatus('ended', msg.data ?? 'the session ended')
            break
        }
      }
      ws.onerror = () => {
        if (stateRef.current !== 'error') setStatus('error', 'connection failed')
      }
      ws.onclose = () => {
        if (stateRef.current === 'watching' || stateRef.current === 'connecting') {
          setStatus('ended', 'disconnected')
        }
      }
    })()
    return () => {
      disposed = true
      ws?.close()
    }
    // getTicket and onStateChange are callbacks from the page; a new one each
    // render must not reopen the socket.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [endpoint, sessionId])

  // Follow the newest statement, as a terminal follows its output.
  useEffect(() => {
    bottomRef.current?.scrollIntoView?.({ block: 'nearest' })
  }, [lines.length])

  return (
    <Stack gap="xs">
      <Group justify="space-between">
        <Group gap="xs">
          {state === 'connecting' && <Loader size="xs" />}
          <Badge color={state === 'watching' ? 'amber' : state === 'error' ? 'rose' : 'slate'} variant="light">
            {state === 'watching' ? 'read only · live' : state}
          </Badge>
        </Group>
        {detail && <Text size="xs" c={state === 'error' ? 'rose' : 'dimmed'}>{detail}</Text>}
      </Group>
      {dropped && (
        <Text size={FS.micro} c="dimmed">
          Showing the latest {WATCH_MAX_LINES.toLocaleString()} recording lines. The replay has them all once the session ends.
        </Text>
      )}
      <Box style={{ border: '1px solid rgba(240,180,41,0.35)', borderRadius: 8, maxHeight: '65vh', overflow: 'auto' }}>
        {lines.length === 0 ? (
          <Text size={FS.meta} c="dimmed" p="md">
            {state === 'watching' ? 'Connected. No statements yet; each one appears here as it is run.' : ' '}
          </Text>
        ) : (
          <PostgresReplay text={lines.join('\n')} live />
        )}
        <div ref={bottomRef} />
      </Box>
    </Stack>
  )
}
