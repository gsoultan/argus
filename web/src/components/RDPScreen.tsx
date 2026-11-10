import { useEffect, useRef, useState } from 'react'
import { ActionIcon, Badge, Box, Button, Group, Loader, Stack, Text, TextInput, Tooltip } from '@mantine/core'
import {
  IconArrowBackUp, IconBell, IconCircle, IconLock, IconPower, IconSend, IconSquare, IconVolume,
  IconVolume2,
} from '@tabler/icons-react'
import { canDecodeH264 } from '~/lib/gfx'
import type { RDPWorkerRequest, RDPWorkerResponse } from '~/workers/rdp.worker'

export type RDPState = 'connecting' | 'live' | 'closed' | 'error'

export interface RDPScreenProps {
  /** The WebSocket endpoint as an http(s) URL: desktopEndpoint, or watchEndpoint. */
  endpoint: string
  target: string
  principal: string
  /** Fetches a ticket. A callback because tickets are single-use. */
  getTicket: () => Promise<string | null>
  /**
   * Hands over the account's password for the logon, once. A callback so the
   * caller can forget it the moment it is sent: it goes to the server's agent
   * for this one sign-in and is kept nowhere.
   */
  takePassword?: () => string
  /** Read-only disables input, for shadowing someone else's session. */
  readOnly?: boolean
  /**
   * When set, attaches to an existing session instead of opening one, on
   * the watch endpoint: there is no input path at all, and the server is
   * asked to redraw so the newcomer sees the screen as it stands rather than
   * only what changes from now on.
   */
  shadowSessionId?: string
  /**
   * An Android device rather than a desktop: the screen is a phone's, shown
   * at its own aspect within the window, and the toolbar has the device's
   * keys and a way to type into it instead of Ctrl+Alt+Del.
   */
  device?: boolean
  onStateChange?: (state: RDPState, detail?: string) => void
  /**
   * Leaves out the status row above the screen, for the session workspace,
   * whose floating toolbar shows the same things. The device keys stay.
   */
  bare?: boolean
  /** The last second's frames, round trip and bandwidth, once a second. */
  onStats?: (stats: RDPStats) => void
  /** Filled with what the workspace's toolbar can do to this screen. */
  controls?: React.MutableRefObject<RDPControls | null>
}

export interface RDPStats { fps: number; rttMs: number; mbit: number; video: boolean }

export interface RDPControls {
  ctrlAltDel: () => void
  focus: () => void
}

/** The device keys a phone has and a keyboard does not, as the agent names them. */
const DEVICE_KEYS: { name: string; label: string; icon: typeof IconArrowBackUp }[] = [
  { name: 'back', label: 'Back', icon: IconArrowBackUp },
  { name: 'home', label: 'Home', icon: IconCircle },
  { name: 'recents', label: 'Recent apps', icon: IconSquare },
  { name: 'notifications', label: 'Notifications', icon: IconBell },
  { name: 'volume_up', label: 'Volume up', icon: IconVolume },
  { name: 'volume_down', label: 'Volume down', icon: IconVolume2 },
  { name: 'power', label: 'Power', icon: IconPower },
  { name: 'lock', label: 'Lock', icon: IconLock },
]

/**
 * The desktop size to ask for: what fits the window, within what Argus
 * offers. The agent rounds it to what RDP accepts.
 */
function wantedSize(): { width: number; height: number } {
  const w = Math.min(Math.max(window.innerWidth - 48, 640), 1920)
  const h = Math.min(Math.max(window.innerHeight - 180, 480), 1200)
  return { width: w - (w % 4), height: h }
}

/**
 * Whether to ask for the graphics pipeline: Windows' own H.264, decoded here.
 * Off unless asked for, with ?graphics=h264 or the argus.desktop.graphics
 * setting, while recordings of it cannot be replayed yet; and only where this
 * browser can decode it.
 */
async function wantsGraphics(): Promise<boolean> {
  let asked = new URLSearchParams(window.location.search).get('graphics') === 'h264'
  try {
    asked ||= window.localStorage.getItem('argus.desktop.graphics') === 'h264'
  } catch {
    // Storage refused: only the URL can ask.
  }
  return asked && (await canDecodeH264())
}

/**
 * The size to show a screen of width by height at, in a room of roomW by
 * roomH: all of it, at its own aspect, and never larger than it is.
 */
export function fitScreen(width: number, height: number, roomW: number, roomH: number): { w: number; h: number } {
  if (width <= 0 || height <= 0) return { w: 0, h: 0 }
  const scale = Math.min(roomW / width, roomH / height, 1)
  return { w: Math.max(Math.floor(width * scale), 1), h: Math.max(Math.floor(height * scale), 1) }
}

/**
 * A Remote Desktop session in a canvas.
 *
 * Decoding and drawing happen in a worker (rdp.worker.ts), which is handed
 * the canvas itself where the browser allows it, so a frame reaches the screen
 * without passing through this thread at all; elsewhere the worker sends
 * finished ImageBitmaps and this thread only blits them. Either way input
 * latency does not depend on how busy the remote desktop is.
 *
 * Memory is one framebuffer. The canvas is the only thing that persists;
 * rectangles are drawn and closed immediately, so a session that runs for hours
 * costs the same as one that just started.
 */
export function RDPScreen({
  endpoint,
  target,
  principal,
  getTicket,
  takePassword,
  readOnly = false,
  shadowSessionId,
  device = false,
  onStateChange,
  bare = false,
  onStats,
  controls,
}: RDPScreenProps) {
  // The canvas is made here rather than rendered by React. A canvas handed
  // to a worker can be handed over once and can no longer be resized from
  // this side, which React would try on every resize; and a reconnect, or
  // StrictMode mounting the effect twice, needs a fresh one.
  const boxRef = useRef<HTMLDivElement>(null)
  const canvasRef = useRef<HTMLCanvasElement | null>(null)
  const [canvas, setCanvas] = useState<HTMLCanvasElement | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  // The desktop's size, for pointing: the canvas's own width is not it once
  // the worker owns the canvas.
  const sizeRef = useRef({ width: 1024, height: 768 })

  const [state, setState] = useState<RDPState>('connecting')
  const stateRef = useRef<RDPState>('connecting')
  const [detail, setDetail] = useState<string>()
  const [size, setSize] = useState({ width: 1024, height: 768 })
  // Frames a second, the round trip and the bandwidth, over the last second.
  // Only a shared console's frames are counted (they carry marks).
  const [stats, setStats] = useState<RDPStats | null>(null)
  // Read from the once-a-second tick without re-arming the session's effect.
  const statsRef = useRef(onStats)
  statsRef.current = onStats

  useEffect(() => {
    const box = boxRef.current
    if (!box) return
    let disposed = false

    const canvas = document.createElement('canvas')
    canvas.width = sizeRef.current.width
    canvas.height = sizeRef.current.height
    canvas.tabIndex = readOnly ? -1 : 0
    Object.assign(canvas.style, {
      // Scaled by the browser rather than by resampling pixels in
      // JavaScript, and never scaled up past its own resolution — the
      // desktop is shown at full fidelity or letterboxed, not blurred. Its
      // size is set by place() below, so the whole screen is always in view.
      display: 'block',
      outline: 'none',
      cursor: readOnly ? 'default' : 'crosshair',
    })
    box.appendChild(canvas)
    canvasRef.current = canvas
    setCanvas(canvas)

    const resizeTo = (width: number, height: number) => {
      sizeRef.current = { width, height }
      setSize({ width, height })
      // The worker sizes a canvas it owns; this side only sets the shape it
      // is laid out at.
      if (!offscreen) {
        canvas.width = width
        canvas.height = height
      }
      place()
    }

    // The whole screen fits the room it has, as wide as the frame and no
    // lower than the window's bottom edge (or the display's, full screen):
    // the page around a session clips what overflows, so a canvas sized by
    // width alone lost the bottom of a screen taller than what was left.
    const place = () => {
      const { width, height } = sizeRef.current
      const frame = box.parentElement ?? box
      const full = document.fullscreenElement === frame
      const roomW = frame.clientWidth || width
      const top = full ? 0 : frame.getBoundingClientRect().top
      // In the workspace the screen runs to the window's edge, with no margin.
      const roomH = Math.max(window.innerHeight - top - (full || bare ? 0 : 16), 160)
      const { w, h } = fitScreen(width, height, roomW, roomH)
      canvas.style.width = `${w}px`
      canvas.style.height = `${h}px`
    }
    const observer = typeof ResizeObserver === 'function' ? new ResizeObserver(place) : null
    observer?.observe(box.parentElement ?? box)
    window.addEventListener('resize', place)
    document.addEventListener('fullscreenchange', place)

    const setStatus = (s: RDPState, d?: string) => {
      stateRef.current = s
      setState(s)
      setDetail(d)
      onStateChange?.(s, d)
    }

    const worker = new Worker(new URL('../workers/rdp.worker.ts', import.meta.url), {
      type: 'module',
    })
    const toWorker = (msg: RDPWorkerRequest, transfer: Transferable[]) => worker.postMessage(msg, transfer)

    // The worker draws straight into the canvas where it can. The 2D context
    // below is only for browsers that cannot hand a canvas to a worker.
    let offscreen = false
    let ctx: CanvasRenderingContext2D | null = null
    if (typeof canvas.transferControlToOffscreen === 'function' && typeof OffscreenCanvas !== 'undefined') {
      try {
        const surface = canvas.transferControlToOffscreen()
        toWorker({ type: 'canvas', canvas: surface }, [surface])
        offscreen = true
      } catch {
        // Fall through to drawing here.
      }
    }
    if (!offscreen) {
      // alpha: false lets the compositor skip blending the canvas against the
      // page, which is measurable on a full-screen desktop.
      // desynchronized: true allows the browser to bypass a compositing frame,
      // which is what keeps pointer movement feeling attached to the cursor.
      ctx = canvas.getContext('2d', { alpha: false, desynchronized: true }) as CanvasRenderingContext2D | null
      if (!ctx) {
        worker.terminate()
        canvas.remove()
        setStatus('error', 'this browser has no 2D canvas')
        return
      }
    }
    resizeTo(sizeRef.current.width, sizeRef.current.height)

    // What the last second was like, shown once a second rather than on
    // every frame, so the page does not re-render at the desktop's rate.
    let frames = 0
    let bytes = 0
    let rttMs = 0
    let marked = false
    let video = false
    const tick = window.setInterval(() => {
      if (marked) {
        const now = { fps: frames, rttMs, mbit: (bytes * 8) / 1e6, video }
        setStats(now)
        statsRef.current?.(now)
      }
      frames = 0
      bytes = 0
      video = false
    }, 1000)

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
        ctx?.drawImage(r.bitmap, r.x, r.y)
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
          resizeTo(m.width, m.height)
          setStatus('live')
          break
        case 'resize':
          resizeTo(m.width, m.height)
          break
        case 'closed':
          // A reason the control plane gave just before stays on screen.
          if (stateRef.current !== 'error') setStatus('closed', 'the session ended')
          break
        case 'error':
          setStatus('error', m.message)
          break
        case 'mark': {
          // Decoded: the agent may send the next frame. Acknowledged here, as
          // the worker finishes, rather than after the paint, so a tab in the
          // background (where animation frames stop) does not stop the
          // screen and its recording. A watcher sends nothing back.
          marked = true
          frames++
          rttMs = m.rttMs
          video ||= m.video
          const ws = wsRef.current
          if (!shadowSessionId && ws && ws.readyState === WebSocket.OPEN) {
            ws.send(JSON.stringify({ type: 'ack', frame: m.id }))
          }
          break
        }
      }
    }

    ;(async () => {
      const ticket = await getTicket()
      if (disposed) return
      if (!ticket) {
        setStatus('error', 'not authorised to open this session')
        return
      }

      const graphics = !shadowSessionId && (await wantsGraphics())
      // Screen sharing sends motion as H.264 to a browser that decodes it,
      // and exact pixels once still; Remote Desktop ignores this.
      const video = !shadowSessionId && (await canDecodeH264())
      if (disposed) return

      const url = new URL(endpoint, window.location.href)
      url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'
      if (!shadowSessionId) {
        url.searchParams.set('target', target)
        url.searchParams.set('principal', principal)
      }
      url.searchParams.set('ticket', ticket)

      const ws = new WebSocket(url.toString())
      // Without this the socket yields Blobs, and every frame would cost an
      // async read plus a copy before the worker could see it.
      ws.binaryType = 'arraybuffer'
      wsRef.current = ws

      if (!shadowSessionId) {
        // The logon is the first message: the account's password, for the
        // server's own Remote Desktop to authenticate, and the size to open at.
        ws.onopen = () => {
          ws.send(
            JSON.stringify({
              type: 'logon',
              password: takePassword?.() ?? '',
              graphics,
              video,
              ...wantedSize(),
            }),
          )
        }
      }
      ws.onmessage = (ev) => {
        if (ev.data instanceof ArrayBuffer) {
          bytes += ev.data.byteLength
          // Transferred, not copied: the main thread gives up the buffer.
          toWorker(ev.data, [ev.data])
          return
        }
        // Text is the control plane saying why: a refusal, or who ended it.
        try {
          const m = JSON.parse(ev.data as string) as { type?: string; data?: string }
          if (m.type === 'error' && m.data) setStatus('error', m.data)
        } catch {
          // Not something this console sends or understands.
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
      window.clearInterval(tick)
      observer?.disconnect()
      window.removeEventListener('resize', place)
      document.removeEventListener('fullscreenchange', place)
      wsRef.current?.close()
      worker.terminate()
      for (const r of pending) r.bitmap.close()
      pending = []
      canvas.remove()
      if (canvasRef.current === canvas) canvasRef.current = null
    }
    // size is state this effect sets; including it would reconnect on resize.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [endpoint, target, principal, shadowSessionId])

  // Whether the canvas takes focus and shows a pointer follows readOnly
  // without reconnecting.
  useEffect(() => {
    if (!canvas) return
    canvas.tabIndex = readOnly ? -1 : 0
    canvas.style.cursor = readOnly ? 'default' : 'crosshair'
  }, [canvas, readOnly])

  /* ── Input ─────────────────────────────────────────────────────────────── */

  useEffect(() => {
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
        x: Math.round(((e.clientX - rect.left) / rect.width) * sizeRef.current.width),
        y: Math.round(((e.clientY - rect.top) / rect.height) * sizeRef.current.height),
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
    const noMenu = (e: MouseEvent) => e.preventDefault()
    canvas.addEventListener('contextmenu', noMenu)
    canvas.addEventListener('keydown', keyDown)
    canvas.addEventListener('keyup', keyUp)

    return () => {
      canvas.removeEventListener('mousemove', onMove)
      canvas.removeEventListener('mousedown', down)
      canvas.removeEventListener('mouseup', up)
      canvas.removeEventListener('wheel', onWheel)
      canvas.removeEventListener('contextmenu', noMenu)
      canvas.removeEventListener('keydown', keyDown)
      canvas.removeEventListener('keyup', keyUp)
    }
  }, [readOnly, canvas])

  /**
   * Ctrl+Alt+Del, which no browser lets a page capture from the keyboard.
   * The sign-in screen of a Windows server asks for it; Remote Desktop
   * passes the keys on as the secure attention sequence, and a shared
   * console's agent sends that sequence itself.
   */
  const frameRef = useRef<HTMLDivElement>(null)
  const [full, setFull] = useState(false)
  useEffect(() => {
    const on = () => setFull(document.fullscreenElement !== null && document.fullscreenElement === frameRef.current)
    document.addEventListener('fullscreenchange', on)
    return () => document.removeEventListener('fullscreenchange', on)
  }, [])
  /** The screen alone on the display, as a remote desktop app shows it. Esc leaves. */
  const toggleFull = () => {
    if (document.fullscreenElement) void document.exitFullscreen().catch(() => undefined)
    else void frameRef.current?.requestFullscreen?.().catch(() => undefined)
    canvasRef.current?.focus()
  }

  const sendCtrlAltDel = () => {
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return
    const codes = ['ControlLeft', 'AltLeft', 'Delete']
    for (const code of codes) ws.send(JSON.stringify({ type: 'key', code, down: true }))
    for (const code of [...codes].reverse()) ws.send(JSON.stringify({ type: 'key', code, down: false }))
    canvasRef.current?.focus()
  }

  if (controls) {
    controls.current = { ctrlAltDel: sendCtrlAltDel, focus: () => canvasRef.current?.focus() }
  }

  /**
   * A device key, which the app plays as the key itself: Back, Home and the
   * rest are not on a keyboard, and Power and Lock are what a thumb does
   * to the side of the phone.
   */
  const sendDeviceKey = (name: string) => {
    const ws = wsRef.current
    if (!ws || ws.readyState !== WebSocket.OPEN) return
    ws.send(JSON.stringify({ type: 'device', name }))
    canvasRef.current?.focus()
  }

  // Typed here and sent whole, into whatever has focus on the device. The
  // canvas takes single keys too; a sentence is easier to see before it
  // goes, and pasting works.
  const [typed, setTyped] = useState('')
  const sendText = () => {
    const ws = wsRef.current
    if (!typed || !ws || ws.readyState !== WebSocket.OPEN) return
    ws.send(JSON.stringify({ type: 'text', text: typed }))
    setTyped('')
    canvasRef.current?.focus()
  }

  return (
    <Stack gap="xs" h={bare ? '100%' : undefined}>
      {!bare && <Group justify="space-between">
        <Group gap="xs">
          {state === 'connecting' && <Loader size="xs" />}
          <Badge
            variant="light"
            color={state === 'live' ? (readOnly ? 'amber' : 'teal') : state === 'error' ? 'rose' : 'slate'}
          >
            {state === 'live' && readOnly ? 'read only · live' : state}
          </Badge>
          <Text size="xs" c="dimmed">
            {size.width}×{size.height}
          </Text>
          {state === 'live' && stats && (
            <Text size="xs" c="dimmed" data-testid="desktop-stats">
              {stats.fps} fps · {stats.rttMs} ms · {stats.mbit.toFixed(1)} Mbit/s
              {stats.video ? ' · H.264' : ' · exact'}
            </Text>
          )}
        </Group>
        <Group gap="xs">
          {detail && (
            <Text size="xs" c="dimmed">
              {detail}
            </Text>
          )}
          {state === 'live' && typeof document.documentElement.requestFullscreen === 'function' && (
            <Button size="compact-xs" variant="default" onClick={toggleFull}>
              {full ? 'Exit full screen' : 'Full screen'}
            </Button>
          )}
          {!readOnly && !device && state === 'live' && (
            <Button size="compact-xs" variant="default" onClick={sendCtrlAltDel}>
              Ctrl+Alt+Del
            </Button>
          )}
        </Group>
      </Group>}
      {device && !readOnly && state === 'live' && (
        <Group gap="xs" wrap="wrap" aria-label="Device keys" data-testid="device-toolbar">
          <Group gap={4} wrap="nowrap">
            {DEVICE_KEYS.map((k) => (
              <Tooltip key={k.name} label={k.label}>
                <ActionIcon variant="default" size="lg" aria-label={k.label} onClick={() => sendDeviceKey(k.name)}>
                  <k.icon size={16} />
                </ActionIcon>
              </Tooltip>
            ))}
          </Group>
          <TextInput
            size="xs"
            placeholder="Type into the device"
            aria-label="Type into the device"
            value={typed}
            onChange={(e) => setTyped(e.currentTarget.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') {
                e.preventDefault()
                sendText()
              }
            }}
            style={{ flex: '1 1 200px' }}
            rightSection={
              <ActionIcon variant="subtle" size="sm" aria-label="Send text" disabled={!typed} onClick={sendText}>
                <IconSend size={14} />
              </ActionIcon>
            }
          />
        </Group>
      )}
      <Box
        ref={frameRef}
        style={{
          background: '#05080c',
          border: '1px solid var(--color-line)',
          borderRadius: 8,
          overflow: 'hidden',
          lineHeight: 0,
          // The screen sits in the middle of the dark surface rather than
          // against its left edge, in a window and full screen alike.
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          // In the workspace the screen has the window to itself: no frame,
          // and it fills what the tab bar leaves.
          ...(bare ? { border: 0, borderRadius: 0, flex: 1, minHeight: 0 } : {}),
        }}
      >
        <div ref={boxRef} style={{ display: 'flex', justifyContent: 'center' }} />
      </Box>
    </Stack>
  )
}
