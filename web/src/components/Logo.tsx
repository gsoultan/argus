import { Box, Group, Text, rem } from '@mantine/core'
import { FS, SP } from '~/theme'

/**
 * The mark: an eye, for Argus Panoptes, the watchman with a hundred of them.
 *
 * It replaced a shield-and-padlock icon on a glowing gradient tile, which is
 * the mark of half the security products on the market and said nothing about
 * this one. The eye says what Argus actually does: it watches every privileged
 * session. Drawn inline so it costs no request and inherits no icon-set style.
 */
export function LogoMark({ size = 26 }: { size?: number }) {
  return (
    <Box
      w={size}
      h={size}
      className="grid place-items-center shrink-0"
      style={{ borderRadius: size * 0.28, background: 'var(--color-brand)' }}
      aria-hidden
    >
      <svg width={size * 0.66} height={size * 0.66} viewBox="0 0 24 24" fill="none">
        <path
          d="M2 12c2.6-4.6 6-7 10-7s7.4 2.4 10 7c-2.6 4.6-6 7-10 7s-7.4-2.4-10-7Z"
          stroke="#fff"
          strokeWidth={2}
          strokeLinejoin="round"
        />
        <circle cx="12" cy="12" r="3.4" fill="#fff" />
      </svg>
    </Box>
  )
}

export function Logo({ markOnly = false, size = 26 }: { markOnly?: boolean; size?: number }) {
  return (
    <Group gap={SP.cozy} wrap="nowrap">
      <LogoMark size={size} />
      {!markOnly && (
        <Box>
          <Text fw={600} size={rem(15)} lh={1.1} c="slate.0">
            Argus
          </Text>
          <Text size={FS.micro} c="dimmed" lh={1.3}>
            Server access, recorded
          </Text>
        </Box>
      )}
    </Group>
  )
}
