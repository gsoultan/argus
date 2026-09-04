import { useCallback, useEffect, useRef, useState } from 'react'
import { Badge, Box, Loader, Progress, Stack, Text, Tooltip } from '@mantine/core'
import { Terminal } from '@xterm/xterm'
import '@xterm/xterm/css/xterm.css'
import { CURSOR_OBSERVING, termFontFamily, termTheme } from '~/lib/termTheme'
import { PlayerControls } from '~/components/PlayerControls'
import type { ReplaySource } from '~/lib/useWorkers'

export interface ReplayProps {
  /** Decoded by the caller, so the frames can also drive a command timeline. */
  decoded: ReplaySource
  /** Called as playback moves, so a sibling panel can highlight the current command. */
  onTimeChange?: (t: number) => void
}

/**
 * Session replay.
 *
 * Rendered by xterm.js — the same emulator that draws a live session, so a
 * recording and the session it recorded cannot disagree about what was on
 * screen. Two things follow from that which the previous renderer could not do:
 * full-screen programs (vim, htop, less) replay as they looked instead of as a
 * stream of redraw escapes, and playback is incremental.
 *
 * Incremental is the important one. The old player recomputed the entire
 * accumulated scrollback, re-parsed every ANSI sequence in it and reconciled one
 * DOM node per style run — on every animation frame. That is work proportional
 * to the whole recording, sixty times a second, so a long session dropped to a
 * few frames per second and pinned a core. Here the terminal holds the screen
 * state and playback hands it only the bytes emitted since the last paint;
 * seeking is the sole operation that costs more, and the worker's keyframes
 * bound even that.
 */
export function Replay({ decoded, onTimeChange }: ReplayProps) {
  const [playing, setPlaying] = useState(false)
  const [time, setTime] = useState(0)
  const [speed, setSpeed] = useState<string>('2')

  const hostRef = useRef<HTMLDivElement>(null)
  const termRef = useRef<Terminal | null>(null)
  const rafRef = useRef<number>(0)
  const lastTickRef = useRef<number>(0)

  /** Where the terminal has actually been painted to, which lags `time`. */
  const drawnTo = useRef(0)
  const inFlight = useRef(false)
  /** Bumped on every seek, so an in-flight advance from before it is discarded. */
  const gen = useRef(0)
  const timeRef = useRef(0)

  const { status, header, duration: total, screen, advance } = decoded

  useEffect(() => {
    timeRef.current = time
  }, [time])

  /* ── Terminal lifecycle ──────────────────────────────────────────────── */

  useEffect(() => {
    if (!hostRef.current || status !== 'ready') return

    const term = new Terminal({
      fontFamily: termFontFamily(),
      fontSize: 12.5,
      lineHeight: 1.35,
      // Nothing is waiting for this viewer to type. A blinking cursor on a
      // recording invites them to try.
      cursorBlink: false,
      disableStdin: true,
      convertEol: false,
      // Bounded on purpose: an unbounded buffer would put a long session's
      // entire output back on the heap, which is what this rewrite removes.
      scrollback: 5_000,
      theme: termTheme(CURSOR_OBSERVING),
    })
    // Matched to the recording's own geometry rather than the viewport. Fitting
    // to the container would rewrap output that was drawn for the subject's
    // terminal, and anything cursor-addressed would render as garbage.
    term.open(hostRef.current)
    term.resize(header?.width ?? 80, header?.height ?? 24)
    termRef.current = term

    return () => {
      term.dispose()
      termRef.current = null
      drawnTo.current = 0
      inFlight.current = false
    }
  }, [status, header?.width, header?.height])

  /* ── Painting ────────────────────────────────────────────────────────── */

  /**
   * Draws forward to wherever the cursor now is.
   *
   * One request in flight at a time. That is the backpressure: if the worker is
   * slower than the animation frame rate the player asks for a bigger slice
   * next time rather than queueing sixty requests a second, which is what the
   * Remote Desktop player used to do.
   */
  const pump = useCallback(async () => {
    const term = termRef.current
    if (!term || inFlight.current) return
    const target = timeRef.current
    if (target <= drawnTo.current) return

    inFlight.current = true
    const mine = gen.current
    const from = drawnTo.current
    const text = await advance(from, target)
    inFlight.current = false
    if (mine !== gen.current || !termRef.current) return // a seek overtook us

    drawnTo.current = target
    if (text) term.write(text)
    if (timeRef.current > drawnTo.current) void pump()
  }, [advance])

  /** Repaints the whole screen at `t`. Only seeking needs this. */
  const repaint = useCallback(
    async (t: number) => {
      const term = termRef.current
      if (!term) return
      const mine = ++gen.current
      inFlight.current = true
      const text = await screen(t)
      inFlight.current = false
      if (mine !== gen.current || !termRef.current) return
      term.reset()
      if (text) term.write(text)
      drawnTo.current = t
    },
    [screen],
  )

  // First paint once decoding finishes, and whenever the terminal is recreated.
  useEffect(() => {
    if (status === 'ready') void repaint(timeRef.current)
  }, [status, repaint])

  useEffect(() => {
    if (time > drawnTo.current) void pump()
  }, [time, pump])

  /**
   * Tells the parent where playback is, at most once a second.
   *
   * `time` changes on every animation frame. Forwarding each one re-rendered
   * the whole session page — command timeline included — sixty times a second
   * to move a highlight that only ever moves between commands. Whole seconds
   * are finer than the timeline can show and 60x less work.
   */
  const lastNotified = useRef(-1)
  useEffect(() => {
    const whole = Math.floor(time)
    if (whole === lastNotified.current) return
    lastNotified.current = whole
    onTimeChange?.(whole)
  }, [time, onTimeChange])

  /* ── Transport ───────────────────────────────────────────────────────── */

  // rAF rather than setInterval so playback stays in step with paint and
  // suspends on its own in a background tab.
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

  const togglePlay = useCallback(() => {
    setPlaying((p) => {
      if (!p && timeRef.current >= total) {
        // Restart rather than sit at the end doing nothing.
        setTime(0)
        void repaint(0)
      }
      return !p
    })
  }, [total, repaint])

  const seek = useCallback(
    (t: number) => {
      const clamped = Math.min(Math.max(0, t), total)
      setTime(clamped)
      // Backwards means replaying from a keyframe; the terminal cannot un-apply
      // bytes it has already been given.
      if (clamped < drawnTo.current) void repaint(clamped)
    },
    [total, repaint],
  )

  /* ── Render ──────────────────────────────────────────────────────────── */

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
      <Box h={420} p="sm" style={{ background: '#05080c', overflow: 'auto' }}>
        <div ref={hostRef} />
      </Box>

      <PlayerControls
        time={time}
        total={total}
        playing={playing}
        speed={speed}
        onSpeed={setSpeed}
        onSeek={seek}
        onPlayPause={togglePlay}
      >
        <Badge size="xs" variant="outline" color="slate" className="argus-digest">
          asciicast v2
        </Badge>
        <Badge size="xs" variant="outline" color="slate" className="argus-digest">
          {header?.width}x{header?.height}
        </Badge>
        <Badge size="xs" variant="outline" color="slate" className="argus-digest">
          {decoded.frameCount.toLocaleString()} frames
        </Badge>
        <Tooltip label="Decoded and indexed off the main thread in a Web Worker, which also holds the frames">
          <Badge size="xs" variant="outline" color="teal" className="argus-digest">
            decoded in {decoded.ms}ms
          </Badge>
        </Tooltip>
      </PlayerControls>
    </Box>
  )
}
