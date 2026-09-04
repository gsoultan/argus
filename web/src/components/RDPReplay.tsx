import { useCallback, useEffect, useRef, useState } from 'react'
import { ActionIcon, Badge, Box, Group, Loader, Slider, Stack, Text } from '@mantine/core'
import { IconPlayerPause, IconPlayerPlay, IconPlayerSkipBack } from '@tabler/icons-react'
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
  const [error, setError] = useState<string>()

  // The last position drawn, so ordinary playback asks only for the new slice
  // rather than replaying from the beginning on every tick.
  const drawnTo = useRef(0)

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
        case 'rects':
          for (const r of m.rects) {
            ctx.drawImage(r.bitmap, r.x, r.y)
            // Closed immediately rather than left to the collector, so a long
            // recording costs the same as a short one.
            r.bitmap.close()
          }
          drawnTo.current = m.toMs
          break
        case 'error':
          setError(m.message)
          break
      }
    }

    // Transferred: the main thread gives up the buffer entirely.
    worker.postMessage({ type: 'load', buffer }, [buffer])

    return () => {
      worker.terminate()
      workerRef.current = null
    }
  }, [buffer])

  // seek asks for the slice needed to reach t.
  //
  // Rectangles are incremental, so moving backwards means clearing and
  // replaying from the start. There is no way to jump into the middle of a
  // stream of deltas without having drawn what came before.
  const seek = useCallback((t: number) => {
    const worker = workerRef.current
    const ctx = ctxRef.current
    if (!worker || !ctx) return

    let from = drawnTo.current
    if (t < drawnTo.current) {
      ctx.fillStyle = '#05080c'
      ctx.fillRect(0, 0, width, height)
      from = 0
      drawnTo.current = 0
    }
    worker.postMessage({ type: 'window', fromMs: from, toMs: t })
  }, [width, height])

  useEffect(() => {
    if (!playing || !ready) return
    let raf = 0
    let last = performance.now()

    const tick = (now: number) => {
      const advanced = now - last
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
  }, [playing, ready, durationMs])

  useEffect(() => {
    if (ready) seek(cursor)
  }, [cursor, ready, seek])

  const time = (ms: number) => {
    const s = Math.floor(ms / 1000)
    return `${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`
  }

  return (
    <Stack gap="xs">
      <Group justify="space-between">
        <Group gap="xs">
          {!ready && !error && <Loader size="xs" />}
          <Badge
            size="sm"
            variant="light"
            color={verified === 'intact' ? 'teal' : verified === 'tampered' ? 'red' : 'slate'}
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

      <Group gap="xs" wrap="nowrap">
        <ActionIcon
          variant="light"
          size="sm"
          disabled={!ready}
          onClick={() => setPlaying((p) => !p)}
          aria-label={playing ? 'Pause' : 'Play'}
        >
          {playing ? <IconPlayerPause size={14} /> : <IconPlayerPlay size={14} />}
        </ActionIcon>
        <ActionIcon
          variant="subtle"
          color="slate"
          size="sm"
          disabled={!ready}
          onClick={() => {
            setPlaying(false)
            setCursor(0)
          }}
          aria-label="Back to start"
        >
          <IconPlayerSkipBack size={14} />
        </ActionIcon>
        <Text size="xs" c="dimmed" ff="monospace" w={44}>
          {time(cursor)}
        </Text>
        <Slider
          flex={1}
          size="sm"
          min={0}
          max={Math.max(durationMs, 1)}
          value={cursor}
          disabled={!ready}
          label={null}
          onChange={(v) => {
            setPlaying(false)
            setCursor(v)
          }}
        />
        <Text size="xs" c="dimmed" ff="monospace" w={44}>
          {time(durationMs)}
        </Text>
      </Group>
    </Stack>
  )
}
