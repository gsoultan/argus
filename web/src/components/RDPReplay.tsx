import { useCallback, useEffect, useRef, useState } from 'react'
import { Badge, Box, Group, Loader, Text } from '@mantine/core'
import { PlayerControls } from '~/components/PlayerControls'
import type { ReplayResponse } from '~/workers/rdpreplay.worker'

export interface RDPReplayProps {
  /** The decoded frame stream from the control plane. */
  buffer: ArrayBuffer
  width: number
  height: number
  /** Chain verdict, shown rather than assumed. */
  verified?: string
}

/**
 * Plays back a recorded Remote Desktop session.
 *
 * The same rendering as a live session — blit finished ImageBitmaps into a
 * canvas — fed from a file rather than a socket. Reusing it is not tidiness: a
 * separate replay renderer would be free to disagree with the live one about
 * what a session showed, and the whole value of the recording is that it does
 * not.
 *
 * Memory is the recording plus one framebuffer. The buffer stays in the worker
 * as the protocol stream it already was; only the rectangles inside the current
 * window are turned into bitmaps, and each is closed the moment it is drawn.
 */
export function RDPReplay({ buffer, width, height, verified }: RDPReplayProps) {
  const canvasRef = useRef<HTMLCanvasElement>(null)
  const workerRef = useRef<Worker | null>(null)
  const ctxRef = useRef<CanvasRenderingContext2D | null>(null)

  const [ready, setReady] = useState(false)
  const [durationMs, setDurationMs] = useState(0)
  const [cursor, setCursor] = useState(0)
  const [playing, setPlaying] = useState(false)
  const [speed, setSpeed] = useState('1')
  const [error, setError] = useState<string>()

  // The last position drawn, so ordinary playback asks only for the new slice
  // rather than replaying from the beginning on every tick.
  const drawnTo = useRef(0)
  /**
   * One request in flight at a time.
   *
   * This player used to post a window request on every animation frame — sixty
   * a second — and the worker answered each by scanning its whole frame index.
   * Now a request is issued only once the previous one has been drawn, so
   * scrubbing costs what the worker can decode rather than what the screen
   * refreshes at.
   */
  const inFlight = useRef(false)
  const gen = useRef(0)

  useEffect(() => {
    const canvas = canvasRef.current
    if (!canvas) return

    const ctx = canvas.getContext('2d', { alpha: false }) as CanvasRenderingContext2D | null
    if (!ctx) {
      setError('this browser has no 2D canvas')
      return
    }
    ctxRef.current = ctx
    ctx.fillStyle = '#05080c'
    ctx.fillRect(0, 0, canvas.width, canvas.height)

    const worker = new Worker(new URL('../workers/rdpreplay.worker.ts', import.meta.url), {
      type: 'module',
    })
    workerRef.current = worker

    worker.onmessage = (ev: MessageEvent<ReplayResponse>) => {
      const m = ev.data
      switch (m.type) {
        case 'loaded':
          setDurationMs(m.durationMs)
          setReady(true)
          break
        case 'rects': {
          inFlight.current = false
          // A window answered from before the last backward seek describes a
          // screen the viewer has already moved away from. Its bitmaps are
          // closed rather than painted over the current one.
          if (m.id !== gen.current) {
            for (const r of m.rects) r.bitmap.close()
            break
          }
          for (const r of m.rects) {
            ctx.drawImage(r.bitmap, r.x, r.y)
            // Closed immediately rather than left to the collector, so a long
            // recording costs the same as a short one.
            r.bitmap.close()
          }
          drawnTo.current = m.toMs
          break
        }
        case 'error':
          inFlight.current = false
          setError(m.message)
          break
      }
    }

    // Transferred: the main thread gives up the buffer entirely.
    worker.postMessage({ type: 'load', buffer }, [buffer])

    return () => {
      worker.terminate()
      workerRef.current = null
      inFlight.current = false
    }
  }, [buffer])

  /**
   * Asks for the slice needed to reach `t`.
   *
   * Rectangles are incremental, so moving backwards means clearing and
   * replaying from the start. There is no way to jump into the middle of a
   * stream of deltas without having drawn what came before.
   */
  const request = useCallback(
    (t: number) => {
      const worker = workerRef.current
      const ctx = ctxRef.current
      if (!worker || !ctx || inFlight.current) return

      let from = drawnTo.current
      if (t < drawnTo.current) {
        ctx.fillStyle = '#05080c'
        ctx.fillRect(0, 0, width, height)
        from = 0
        drawnTo.current = 0
      }
      if (t <= from && t !== 0) return

      inFlight.current = true
      worker.postMessage({ type: 'window', id: ++gen.current, fromMs: from, toMs: t })
    },
    [width, height],
  )

  useEffect(() => {
    if (ready) request(cursor)
  }, [cursor, ready, request])

  // rAF drives the clock; `request` decides when the worker is asked for pixels.
  useEffect(() => {
    if (!playing || !ready) return
    let raf = 0
    let last = performance.now()
    const rate = Number(speed)

    const tick = (now: number) => {
      const advanced = (now - last) * rate
      last = now
      setCursor((c) => {
        const next = c + advanced
        if (next >= durationMs) {
          setPlaying(false)
          return durationMs
        }
        return next
      })
      raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(raf)
  }, [playing, ready, durationMs, speed])

  // The shared transport works in seconds; the display stream is timestamped in
  // milliseconds, so the conversion lives here rather than in both players.
  const seekSeconds = useCallback(
    (secs: number) => setCursor(Math.min(Math.max(0, secs * 1000), durationMs)),
    [durationMs],
  )

  return (
    <Box>
      <Group justify="space-between" mb="xs">
        <Group gap="xs">
          {!ready && !error && <Loader size="xs" />}
          <Badge
            size="sm"
            variant={verified === 'tampered' ? 'filled' : 'light'}
            color={verified === 'intact' ? 'teal' : verified === 'tampered' ? 'rose' : 'slate'}
          >
            {verified === 'intact'
              ? 'chain verified'
              : verified === 'tampered'
                ? 'TAMPERED'
                : 'unverified'}
          </Badge>
          <Text size="xs" c="dimmed">
            {width}×{height}
          </Text>
        </Group>
        {error && (
          <Text size="xs" c="rose.4">
            {error}
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
          width={width}
          height={height}
          style={{ width: '100%', height: 'auto', maxWidth: width, display: 'block' }}
        />
      </Box>

      <PlayerControls
        time={cursor / 1000}
        total={durationMs / 1000}
        playing={playing}
        speed={speed}
        disabled={!ready}
        onSpeed={setSpeed}
        onSeek={seekSeconds}
        onPlayPause={() =>
          cursor >= durationMs ? (setCursor(0), setPlaying(true)) : setPlaying((p) => !p)
        }
      >
        <Badge size="xs" variant="outline" color="slate" className="argus-digest">
          display stream
        </Badge>
        <Badge size="xs" variant="outline" color="slate" className="argus-digest">
          {width}x{height}
        </Badge>
      </PlayerControls>
    </Box>
  )
}
