import { Box, Loader, Stack, Text } from '@mantine/core'

/** Placeholder while a lazily-loaded player chunk arrives. */
export function PlayerFallback({ label }: { label: string }) {
  return (
    <Box className="grid place-items-center" h={420} style={{ background: '#05080c' }}>
      <Stack align="center" gap="xs">
        <Loader size="sm" color="teal" />
        <Text size="xs" c="dimmed">{label}</Text>
      </Stack>
    </Box>
  )
}
