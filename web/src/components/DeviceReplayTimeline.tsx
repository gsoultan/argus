import { Box, Group, ScrollArea, Text, UnstyledButton } from '@mantine/core'
import {
  IconApps,
  IconDeviceMobileOff,
  IconEyeOff,
  IconKeyboard,
  IconLock,
  IconLockOpen,
  IconUserCheck,
} from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import type { ReplayActivity } from '~/types/domain'

/**
 * The device's activity timeline beside the replay: what the phone was doing
 * while the screen showed what it showed -- the app in front, the screen
 * locking, an app hiding its window, text typed. Chained into the recording
 * next to the frames, so an auditor sees the context, not only the pixels.
 *
 * A click seeks the player to that moment. The event at the cursor is
 * highlighted, so scrubbing the video moves the highlight here too.
 */
export function DeviceReplayTimeline({
  activity,
  cursorMs,
  onSeek,
}: {
  activity: ReplayActivity[]
  cursorMs: number
  onSeek: (ms: number) => void
}) {
  if (activity.length === 0) return null

  // The current event is the last one at or before the cursor.
  let currentIndex = -1
  for (let i = 0; i < activity.length; i++) {
    const ev = activity[i]
    if (ev && ev.ms <= cursorMs) currentIndex = i
    else break
  }

  return (
    <Box mt="md">
      <Text size={FS.meta} fw={600} c="dimmed" mb={SP.snug} tt="uppercase">
        Activity
      </Text>
      <ScrollArea.Autosize mah={240} type="hover">
        <Box>
          {activity.map((ev, i) => (
            <UnstyledButton
              key={`${ev.ms}-${i}`}
              onClick={() => onSeek(ev.ms)}
              w="100%"
              px={SP.snug}
              py={4}
              style={{
                display: 'block',
                borderRadius: 6,
                background: i === currentIndex ? 'var(--mantine-color-dark-5)' : undefined,
              }}
            >
              <Group gap={SP.snug} wrap="nowrap">
                <Text size={FS.meta} c="dimmed" ff="monospace" style={{ width: 54, flexShrink: 0 }}>
                  {formatMs(ev.ms)}
                </Text>
                <ActivityIcon kind={ev.kind} />
                <Text size={FS.meta} style={{ lineHeight: 1.3 }}>
                  {describe(ev)}
                </Text>
              </Group>
            </UnstyledButton>
          ))}
        </Box>
      </ScrollArea.Autosize>
    </Box>
  )
}

function ActivityIcon({ kind }: { kind: string }) {
  const size = 15
  const props = { size, style: { flexShrink: 0 } }
  switch (kind) {
    case 'app':
      return <IconApps {...props} />
    case 'lock':
      return <IconLock {...props} />
    case 'unlock':
      return <IconLockOpen {...props} />
    case 'secure':
      return <IconEyeOff {...props} />
    case 'typed':
      return <IconKeyboard {...props} />
    case 'consent':
      return <IconUserCheck {...props} />
    case 'ended':
      return <IconDeviceMobileOff {...props} />
    default:
      return <IconApps {...props} />
  }
}

/** A plain-English line for one event. Exported for the test. */
export function describe(ev: ReplayActivity): string {
  switch (ev.kind) {
    case 'app':
      return ev.title
        ? `${ev.title}${ev.package ? ` (${ev.package})` : ''} came to the front`
        : `${ev.package || 'An app'} came to the front`
    case 'lock':
      return 'Screen locked'
    case 'unlock':
      return 'Screen unlocked'
    case 'secure':
      return ev.hidden ? 'Secure content hidden — the screen went black' : 'Secure content ended'
    case 'typed':
      // A password's text is never in the recording; the timeline says so.
      return ev.password ? 'Typed into a password field' : `Typed “${ev.text ?? ''}”`
    case 'consent':
      return ev.by ? `${ev.by} accepted the session on the device` : 'The session was accepted on the device'
    case 'ended':
      return 'The session was ended on the device'
    default:
      return ev.kind
  }
}

function formatMs(ms: number): string {
  const total = Math.floor(ms / 1000)
  const m = Math.floor(total / 60)
  const s = total % 60
  return `${m}:${s.toString().padStart(2, '0')}`
}
