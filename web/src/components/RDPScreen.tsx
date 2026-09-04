import { useEffect, useRef, useState } from 'react'
import { Badge, Box, Group, Loader, Stack, Text } from '@mantine/core'
import type { RDPWorkerResponse } from '~/workers/rdp.worker'

export type RDPState = 'connecting' | 'live' | 'closed' | 'error'

export interface RDPScreenProps {
  gatewayUrl: string
  target: string
  principal: string
  /** Fetches a ticket. A callback because tickets are single-use. */
  getTicket: () => Promise<string | null>
  /** Read-only disables input, for shadowing someone else's session. */
  readOnly?: boolean
  /**
   * When set, attaches to an existing session instead of opening one.
   *
   * Shadowing uses a different endpoint because the gateway must not treat a
   * viewer as a second operator: there is no input path at all, and the target
   * is asked to redraw so the newcomer sees the screen as it stands rather than
   * only what changes from now on.
   */
  shadowSessionId?: string
  onStateChange?: (state: RDPState, detail?: string) => void
}

/**
 * A Remote Desktop session in a canvas.
 *
 * The main thread does one thing: blit finished ImageBitmaps into a canvas.
 * Decoding happens in the gateway, image construction happens in a worker, and
 * neither touches this thread — so input latency does not depend on how busy
 * the remote desktop is.
 *
 * Memory is one framebuffer. The canvas is the only thing that persists;
 * rectangles are drawn and closed immediately, so a session that runs for hours
 * costs the same as one that just started.
 */
export function RDPScreen({
  gatewayUrl,
  target,
  principal,
  getTicket,
  readOnly = false,
  shadowSessionId,
  onStateChange,
}: RDPScreenProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const ctxRef = useRef<CanvasRenderingContext2D | null>(null)

  const [state, setState] = useState<RDPState>('connecting')
  const stateRef = useRef<RDPState>('connecting')
  const [detail, setDetail] = useState<string>()
  const [size, setSize] = useState({ width: 1024, height: 768 })

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas) return
    let disposed = false

    const setStatus = (s: RDPState, d?: string) => {
      stateRef.current = s
      setState(s)
      setDetail(d)
      onStateChange?.(s, d)
    }

    // alpha: false lets the compositor skip blending the canvas against the
    // page, which is measurable on a full-screen desktop.
    // desynchronized: true allows the browser to bypass a compositing frame,
    // which is what keeps pointer movement feeling attached to the cursor.
    const ctx = canvas.getContext('2d', {
      alpha: false,
      desynchronized: true,
    }) as CanvasRenderingContext2D | null
    if (!ctx) {
      setStatus('error', 'this browser has no 2D canvas')
      return
    }
    ctxRef.current = ctx

    const worker = new Worker(new URL('../workers/rdp.worker.ts', import.meta.url), {
      type: 'module',
    })

    // Rectangles are queued and drawn in one animation frame rather than one at
    // a time. A desktop update is often a dozen rectangles; drawing each on
    // arrival means a dozen separate paints for one visual change.
    let pending: Array<{ x: number; y: number; bitmap: ImageBitmap }> = []
    let scheduled = false
    const flush = () => {
      scheduled = false
      const batch = pending
      pending = []
      for (const r of batch) {
        ctx.drawImage(r.bitmap, r.x, r.y)
        // Closing releases the decoded pixels immediately instead of waiting
        // for the collector, which is what stops memory tracking session
        // length rather than screen size.
        r.bitmap.close()
      }
    }

    worker.onmessage = (ev: MessageEvent<RDPWorkerResponse>) => {
      const m = ev.data
      switch (m.type) {
        case 'rects':
          for (const r of m.rects) pending.push({ x: r.x, y: r.y, bitmap: r.bitmap })
          if (!scheduled) {
            scheduled = true
            requestAnimationFrame(flush)
          }
          break
        case 'ready':
          setSize({ width: m.width, height: m.height })
          setStatus('live')
          break
        case 'resize':
          setSize({ width: m.width, height: m.height })
          break
        case 'closed':
          setStatus('closed', 'the session ended')
          break
        case 'error':
          setStatus('error', m.message)
          break
      }
    }

    ;(async () => {
      const ticket = await getTicket()
      if (disposed) return
      if (!ticket) {
        setStatus('error', 'not authorised to open this session')
        return
      }

      const url = new URL(shadowSessionId ? '/ws/rdp/shadow' : '/ws/rdp', gatewayUrl)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      if (shadowSessionId) {
        url.searchParams.set('session', shadowSessionId)
      } else {
        url.searchParams.set('target', target)
        url.searchParams.set('principal', principal)
      }
      url.searchParams.set('ticket', ticket)

      const ws = new WebSocket(url.toString())
      // Without this the socket yields Blobs, and every frame would cost an
      // async read plus a copy before the worker could see it.
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws

      ws.onmessage = (ev) => {
        if (ev.data instanceof ArrayBuffer) {
          // Transferred, not copied: the main thread gives up the buffer.
          worker.postMessage(ev.data, [ev.data])
        }
      }
      ws.onerror = () => setStatus('error', 'connection failed')
      ws.onclose = () => {
        if (stateRef.current === 'live' || stateRef.current === 'connecting') {
          setStatus('closed')
        }
      }
    })()

    return () => {
      disposed = true
      wsRef.current?.close()
      worker.terminate()
      for (const r of pending) r.bitmap.close()
      pending = []
    }
    // size is state this effect sets; including it would reconnect on resize.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [gatewayUrl, target, principal, shadowSessionId])

  /* ── Input ─────────────────────────────────────────────────────────────── */

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas || readOnly) return

    const send = (payload: object) => {
      const ws = wsRef.current
      if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(payload))
    }

    // Coordinates are scaled from the element's rendered size back to the
    // desktop's, so a canvas shown smaller than the remote screen still points
    // at the pixel the user is looking at.
    const toDesktop = (e: MouseEvent) => {
      const rect = canvas.getBoundingClientRect()
      return {
        x: Math.round(((e.clientX - rect.left) / rect.width) * canvas.width),
        y: Math.round(((e.clientY - rect.top) / rect.height) * canvas.height),
      }
    }

    // Pointer movement is throttled to one animation frame. A mouse reports far
    // more often than a screen refreshes, and sending every sample floods the
    // link without the user seeing any of the extra positions.
    let queued: { x: number; y: number } | null = null
    let pending = false
    const onMove = (e: MouseEvent) => {
      queued = toDesktop(e)
      if (pending) return
      pending = true
      requestAnimationFrame(() => {
        pending = false
        if (queued) send({ type: 'mouse', ...queued })
      })
    }

    const onButton = (e: MouseEvent, down: boolean) => {
      e.preventDefault()
      send({ type: 'button', button: e.button, down, ...toDesktop(e) })
    }
    const onWheel = (e: WheelEvent) => {
      e.preventDefault()
      send({ type: 'wheel', delta: e.deltaY, ...toDesktop(e) })
    }
    const onKey = (e: KeyboardEvent, down: boolean) => {
      // The browser's own shortcuts are left alone; capturing them would trap
      // the user inside the canvas with no way to close the tab.
      if (e.metaKey && !down) return
      e.preventDefault()
      send({ type: 'key', code: e.code, down })
    }

    const down = (e: MouseEvent) => onButton(e, true)
    const up = (e: MouseEvent) => onButton(e, false)
    const keyDown = (e: KeyboardEvent) => onKey(e, true)
    const keyUp = (e: KeyboardEvent) => onKey(e, false)

    canvas.addEventListener('mousemove', onMove)
    canvas.addEventListener('mousedown', down)
    canvas.addEventListener('mouseup', up)
    canvas.addEventListener('wheel', onWheel, { passive: false })
    canvas.addEventListener('contextmenu', (e) => e.preventDefault())
    canvas.addEventListener('keydown', keyDown)
    canvas.addEventListener('keyup', keyUp)

    return () => {
      canvas.removeEventListener('mousemove', onMove)
      canvas.removeEventListener('mousedown', down)
      canvas.removeEventListener('mouseup', up)
      canvas.removeEventListener('wheel', onWheel)
      canvas.removeEventListener('keydown', keyDown)
      canvas.removeEventListener('keyup', keyUp)
    }
  }, [readOnly])

  return (
    <Stack gap="xs">
      <Group justify="space-between">
        <Group gap="xs">
          {state === 'connecting' && <Loader size="xs" />}
          <Badge
            variant="light"
            color={state === 'live' ? (readOnly ? 'yellow' : 'teal') : state === 'error' ? 'red' : 'gray'}
          >
            {state === 'live' && readOnly ? 'read only · live' : state}
          </Badge>
          <Text size="xs" c="dimmed">
            {size.width}×{size.height}
          </Text>
        </Group>
        {detail && (
          <Text size="xs" c="dimmed">
            {detail}
          </Text>
        )}
      </Group>
      <Box
        style={{
          background: '#05080c',
          border: '1px solid var(--color-line)',
          borderRadius: 8,
          overflow: 'hidden',
          lineHeight: 0,
        }}
      >
        <canvas
          ref={canvasRef}
          width={size.width}
          height={size.height}
          tabIndex={readOnly ? -1 : 0}
          style={{
            // Scaled by the browser rather than by resampling pixels in
            // JavaScript, and never scaled up past its own resolution — the
            // desktop is shown at full fidelity or letterboxed, not blurred.
            width: '100%',
            height: 'auto',
            maxWidth: size.width,
            display: 'block',
            outline: 'none',
            cursor: readOnly ? 'default' : 'crosshair',
          }}
        />
      </Box>
    </Stack>
  )
}
