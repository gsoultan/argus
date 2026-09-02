import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ActionIcon, Badge, Box, Group, Loader, Progress, ScrollArea, Select, Slider,
  Stack, Text, Tooltip,
} from '@mantine/core'
import {
  IconPlayerPause, IconPlayerPlay, IconPlayerSkipBack, IconPlayerSkipForward,
} from '@tabler/icons-react'
import { toSpans } from '~/lib/ansi'
import type { ReplayState } from '~/lib/useWorkers'

const SPEEDS = ['0.5', '1', '2', '4', '8'] as const

function clock(secs: number): string {
  const s = Math.max(0, Math.floor(secs))
  return `${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`
}

export interface ReplayProps {
  /** Decoded by the caller, so the frames can also drive a command timeline. */
  decoded: ReplayState
  /** Called as playback moves, so a sibling panel can highlight the current command. */
  onTimeChange?: (t: number) => void
}

export function Replay({ decoded, onTimeChange }: ReplayProps) {
  const [playing, setPlaying] = useState(false)
  const [time, setTime] = useState(0)
  const [speed, setSpeed] = useState<string>('2')

  const viewportRef = useRef<HTMLDivElement>(null)
  const rafRef = useRef<number>(0)
  const lastTickRef = useRef<number>(0)
  const followRef = useRef(true)

  const { frames, keyframes, duration: total, header, status } = decoded

  /**
   * Screen contents at `time`.
   *
   * Terminal output is cumulative, so seeking means replaying every byte up to
   * the target. The worker gave us keyframes — accumulated text snapshots taken
   * every few seconds — so we start from the nearest one and only apply the
   * remainder. Without this, dragging the scrubber on a long session is O(n)
   * per frame and the UI locks up.
   */
  const screen = useMemo(() => {
    if (frames.length === 0) return ''

    let base = ''
    let start = 0
    for (let i = keyframes.length - 1; i >= 0; i--) {
      const kf = keyframes[i]!
      if (kf.t <= time) {
        base = kf.text
        start = kf.frameIndex + 1
        break
      }
    }

    let tail = ''
    for (let i = start; i < frames.length; i++) {
      const f = frames[i]!
      if (f.t > time) break
      if (f.kind === 'o') tail += f.data
    }
    return base + tail
  }, [frames, keyframes, time])

  const spans = useMemo(() => toSpans(screen), [screen])

  // Drive playback off rAF rather than setInterval so it stays in step with
  // paint and pauses automatically in a background tab.
  useEffect(() => {
    if (!playing) return
    lastTickRef.current = performance.now()
    const rate = Number(speed)

    const tick = (now: number) => {
      const delta = (now - lastTickRef.current) / 1000
      lastTickRef.current = now
      setTime((t) => {
        const next = t + delta * rate
        if (next >= total) {
          setPlaying(false)
          return total
        }
        return next
      })
      rafRef.current = requestAnimationFrame(tick)
    }

    rafRef.current = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(rafRef.current)
  }, [playing, speed, total])

  useEffect(() => {
    onTimeChange?.(time)
  }, [time, onTimeChange])

  // Keep the newest output in view while playing, but stop fighting the user
  // if they have scrolled up to read something.
  useEffect(() => {
    if (!followRef.current) return
    const el = viewportRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [spans])

  const onScrollPositionChange = useCallback(({ y }: { y: number }) => {
    const el = viewportRef.current
    if (!el) return
    followRef.current = y + el.clientHeight >= el.scrollHeight - 40
  }, [])

  const seek = useCallback(
    (t: number) => {
      followRef.current = true
      setTime(Math.min(Math.max(0, t), total))
    },
    [total],
  )

  // Space to play/pause, arrows to nudge — muscle memory from every other player.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement | null
      if (target && ['INPUT', 'TEXTAREA'].includes(target.tagName)) return
      if (e.code === 'Space') {
        e.preventDefault()
        setPlaying((p) => !p)
      } else if (e.code === 'ArrowLeft') {
        seek(time - (e.shiftKey ? 30 : 5))
      } else if (e.code === 'ArrowRight') {
        seek(time + (e.shiftKey ? 30 : 5))
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [seek, time])

  if (status === 'decoding' || status === 'idle') {
    return (
      <Box className="grid place-items-center" h={420} style={{ background: '#05080c' }}>
        <Stack align="center" gap="xs">
          <Loader size="sm" color="teal" />
          <Text size="xs" c="dimmed">Decoding recording…</Text>
          {decoded.progress > 0 && (
            <Progress value={decoded.progress * 100} size="xs" w={180} color="teal" />
          )}
        </Stack>
      </Box>
    )
  }

  if (status === 'failed') {
    return (
      <Box className="grid place-items-center" h={420} style={{ background: '#05080c' }}>
        <Text size="xs" c="rose.4">Recording could not be decoded: {decoded.error}</Text>
      </Box>
    )
  }

  return (
    <Box>
      <ScrollArea
        h={420}
        viewportRef={viewportRef}
        onScrollPositionChange={onScrollPositionChange}
        style={{ background: '#05080c' }}
        scrollbarSize={8}
      >
        <Box p="md" className="argus-term" c="slate.1">
          {spans.map((s, i) => (
            <span
              key={i}
              style={{
                color: s.fg,
                backgroundColor: s.bg,
                fontWeight: s.bold ? 700 : undefined,
                opacity: s.dim ? 0.55 : undefined,
                fontStyle: s.italic ? 'italic' : undefined,
                textDecoration: s.underline ? 'underline' : undefined,
              }}
            >
              {s.text}
            </span>
          ))}
          {playing && (
            <Box
              component="span"
              w={7}
              h={14}
              display="inline-block"
              className="animate-pulse"
              style={{ background: 'var(--color-verified)', verticalAlign: 'text-bottom' }}
            />
          )}
        </Box>
      </ScrollArea>

      <Box p="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
        <Group gap="sm" wrap="nowrap">
          <ActionIcon
            variant="filled"
            color="teal"
            radius="xl"
            size="lg"
            onClick={() => (time >= total ? (seek(0), setPlaying(true)) : setPlaying((p) => !p))}
          >
            {playing ? <IconPlayerPause size={16} /> : <IconPlayerPlay size={16} />}
          </ActionIcon>

          <Tooltip label="Back 10s">
            <ActionIcon variant="subtle" color="slate" onClick={() => seek(time - 10)}>
              <IconPlayerSkipBack size={15} />
            </ActionIcon>
          </Tooltip>
          <Tooltip label="Forward 10s">
            <ActionIcon variant="subtle" color="slate" onClick={() => seek(time + 10)}>
              <IconPlayerSkipForward size={15} />
            </ActionIcon>
          </Tooltip>

          <Text size="xs" c="dimmed" ff="monospace" w={92} ta="center">
            {clock(time)} / {clock(total)}
          </Text>

          <Slider
            flex={1}
            value={time}
            onChange={seek}
            min={0}
            max={total || 1}
            step={0.1}
            label={(v) => clock(v)}
            color="teal"
            size="sm"
            styles={{ track: { cursor: 'pointer' } }}
          />

          <Select
            size="xs"
            w={78}
            value={speed}
            onChange={(v) => setSpeed(v ?? '1')}
            data={SPEEDS.map((s) => ({ value: s, label: `${s}x` }))}
            allowDeselect={false}
            comboboxProps={{ withinPortal: true }}
          />
        </Group>

        <Group gap="xs" mt={7}>
          <Badge size="xs" variant="outline" color="slate" className="argus-digest">
            asciicast v2
          </Badge>
          <Badge size="xs" variant="outline" color="slate" className="argus-digest">
            {header?.width}x{header?.height}
          </Badge>
          <Badge size="xs" variant="outline" color="slate" className="argus-digest">
            {frames.length.toLocaleString()} frames
          </Badge>
          <Tooltip label="Decoded and indexed off the main thread in a Web Worker">
            <Badge size="xs" variant="outline" color="teal" className="argus-digest">
              decoded in {decoded.ms}ms
            </Badge>
          </Tooltip>
          <Text size="10px" c="dimmed">
            Space to play/pause · ←/→ to seek · Shift for 30s
          </Text>
        </Group>
      </Box>
    </Box>
  )
}
