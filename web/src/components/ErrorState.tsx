import { Box, Button, Code, Group, Stack, Text, ThemeIcon } from '@mantine/core'
import { IconAlertTriangle, IconRefresh } from '@tabler/icons-react'

/**
 * What a route shows when it throws.
 *
 * There were no error boundaries anywhere, so any render-time throw blanked the
 * console to a white page — in an application whose users reach for it when
 * something is already going wrong. A failure has to stay inside the shell,
 * name itself, and leave a way out.
 *
 * The message is shown rather than swallowed. This console is operated by
 * people who run infrastructure; "something went wrong" wastes the one piece of
 * information that would let them tell a bug from an outage.
 */
export function ErrorState({
  error,
  reset,
  title = 'This page could not be rendered',
}: {
  error: unknown
  reset?: () => void
  title?: string
}) {
  const message =
    error instanceof Error ? error.message : typeof error === 'string' ? error : 'Unknown error'

  return (
    <Box className="grid place-items-center" mih="60vh" p="lg">
      <Stack align="center" gap="sm" maw={520}>
        <ThemeIcon variant="light" color="rose" size={44} radius="md">
          <IconAlertTriangle size={22} />
        </ThemeIcon>
        <Text fw={600} size="sm" ta="center">{title}</Text>
        <Text size="xs" c="dimmed" ta="center">
          Nothing was changed on the gateway or in the audit log — this failed while drawing
          the page, not while carrying out an action.
        </Text>
        <Code block fz="xs" w="100%" style={{ whiteSpace: 'pre-wrap' }}>
          {message}
        </Code>
        <Group gap="xs" mt={4}>
          {reset && (
            <Button size="xs" variant="light" color="teal" leftSection={<IconRefresh size={14} />}
              onClick={reset}>
              Try again
            </Button>
          )}
          {/* A document navigation, not a router one: leaving an error
              boundary should discard the state that produced the error, and a
              client-side transition would carry it along. */}
          <Button size="xs" variant="subtle" color="slate" component="a" href="/">
            Back to overview
          </Button>
        </Group>
      </Stack>
    </Box>
  )
}
