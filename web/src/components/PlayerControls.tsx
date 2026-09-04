import { useEffect, useRef } from 'react'
import {
  ActionIcon, Box, Group, Select, Slider, Text, Tooltip,
} from '@mantine/core'
import {
  IconPlayerPause, IconPlayerPlay, IconPlayerSkipBack, IconPlayerSkipForward,
} from '@tabler/icons-react'

export const SPEEDS = ['0.5', '1', '2', '4', '8'] as const

export function clock(secs: number): string {
  const s = Math.max(0, Math.floor(secs))
  return `${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`
}

export interface PlayerControlsProps {
  /** Seconds. Both players work in seconds; the RDP one converts at its edges. */
  time: number
  total: number
  playing: boolean
  speed: string
  disabled?: boolean
  onPlayPause: () => void
  onSeek: (t: number) => void
  onSpeed: (s: string) => void
  /** Format badges rendered under the transport. */
  children?: React.ReactNode
}

/**
 * The transport shared by both replay players.
 *
 * They were written separately and diverged: terminal replay had five playback
 * speeds, ten-second skips and keyboard shortcuts; desktop replay had none of
 * them and ran only at 1×. Watching the same incident across two protocols
 * meant learning two players. There is one now, and a capability added to it
 * arrives in both.
 */
export function PlayerControls({
  time, total, playing, speed, disabled, onPlayPause, onSeek, onSpeed, children,
}: PlayerControlsProps) {
  // The listener reads the current position through refs rather than closing
  // over it. Depending on `time` directly would tear down and re-add a window
  // listener on every animation frame of playback.
  const latest = useRef({ time, onPlayPause, onSeek })
  latest.current = { time, onPlayPause, onSeek }

  // Muscle memory from every other player. Ignored while typing, so a
  // termination reason containing a space does not pause the recording.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null
      if (el && (['INPUT', 'TEXTAREA'].includes(el.tagName) || el.isContentEditable)) return
      const { time: at, onPlayPause: toggle, onSeek: seek } = latest.current
      if (e.code === 'Space') {
        e.preventDefault()
        toggle()
      } else if (e.code === 'ArrowLeft') {
        e.preventDefault()
        seek(at - (e.shiftKey ? 30 : 5))
      } else if (e.code === 'ArrowRight') {
        e.preventDefault()
        seek(at + (e.shiftKey ? 30 : 5))
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  return (
    <Box p="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
      <Group gap="sm" wrap="nowrap">
        <ActionIcon
          variant="filled"
          color="teal"
          radius="xl"
          size="lg"
          disabled={disabled}
          aria-label={playing ? 'Pause' : 'Play'}
          onClick={onPlayPause}
        >
          {playing ? <IconPlayerPause size={16} /> : <IconPlayerPlay size={16} />}
        </ActionIcon>

        <Tooltip label="Back 10s">
          <ActionIcon
            aria-label="Back 10 seconds"
            variant="subtle"
            color="slate"
            disabled={disabled}
            onClick={() => onSeek(time - 10)}
          >
            <IconPlayerSkipBack size={15} />
          </ActionIcon>
        </Tooltip>
        <Tooltip label="Forward 10s">
          <ActionIcon
            aria-label="Forward 10 seconds"
            variant="subtle"
            color="slate"
            disabled={disabled}
            onClick={() => onSeek(time + 10)}
          >
            <IconPlayerSkipForward size={15} />
          </ActionIcon>
        </Tooltip>

        <Text size="xs" c="dimmed" ff="monospace" w={92} ta="center">
          {clock(time)} / {clock(total)}
        </Text>

        <Slider
          flex={1}
          value={time}
          onChange={onSeek}
          min={0}
          max={total || 1}
          step={0.1}
          label={(v) => clock(v)}
          color="teal"
          size="sm"
          disabled={disabled}
          // Mantine renders the thumb as a div; without this it is the one
          // control on the page a screen reader cannot name.
          thumbLabel="Seek through the recording"
          styles={{ track: { cursor: 'pointer' } }}
        />

        <Select
          size="xs"
          w={78}
          value={speed}
          onChange={(v) => onSpeed(v ?? '1')}
          data={SPEEDS.map((s) => ({ value: s, label: `${s}x` }))}
          allowDeselect={false}
          disabled={disabled}
          aria-label="Playback speed"
          comboboxProps={{ withinPortal: true }}
        />
      </Group>

      <Group gap="xs" mt={8}>
        {children}
        <Text size="xs" c="dimmed">
          Space to play/pause · ←/→ to seek · Shift for 30s
        </Text>
      </Group>
    </Box>
  )
}
