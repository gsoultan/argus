import { Box, Button, Code, Group, Stack, Text, ThemeIcon } from '@mantine/core'
import { IconAlertTriangle, IconLogin, IconRefresh } from '@tabler/icons-react'
import { ControlPlaneError } from '~/lib/live'
import { NetworkError, errorText } from '~/lib/apiError'

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
 *
 * It is said in words, though. A request the control plane refused shows what
 * that means and what to do, with the path and status underneath for whoever
 * needs them -- not "/api/v1/databases → 401" as the whole message, and not
 * under a heading that blames the drawing of the page.
 */
export function ErrorState({
  error,
  reset,
  title,
}: {
  error: unknown
  reset?: () => void
  title?: string
}) {
  const refused = error instanceof ControlPlaneError ? error : null
  const fetched = refused !== null || error instanceof NetworkError
  const message = errorText(error, 'Unknown error')
  const heading = title ?? (refused?.signedOut
    ? 'You are signed out'
    : fetched ? 'This page could not be loaded' : 'This page could not be rendered')

  return (
    <Box className="grid place-items-center" mih="60vh" p="lg">
      <Stack align="center" gap="sm" maw={520}>
        <ThemeIcon variant="light" color="rose" size={44} radius="md">
          <IconAlertTriangle size={22} />
        </ThemeIcon>
        <Text fw={600} size="sm" ta="center">{heading}</Text>
        {fetched ? (
          <Text size="sm" ta="center">{message}</Text>
        ) : (
          <>
            <Text size="xs" c="dimmed" ta="center">
              Nothing was changed on any server or in the audit log — this failed while drawing
              the page, not while carrying out an action.
            </Text>
            <Code block fz="xs" w="100%" style={{ whiteSpace: 'pre-wrap' }}>
              {message}
            </Code>
          </>
        )}
        {refused && (
          <Text size="xs" c="dimmed" ta="center" ff="monospace">
            {refused.path} · {refused.status}
          </Text>
        )}
        <Group gap="xs" mt={4}>
          {refused?.signedOut && (
            // A document load, so the sign-in gate starts from scratch and
            // brings the person back to this same address afterwards.
            <Button size="xs" leftSection={<IconLogin size={14} />}
              onClick={() => window.location.reload()}>
              Sign in again
            </Button>
          )}
          {reset && !refused?.signedOut && (
            <Button size="xs" variant="light" leftSection={<IconRefresh size={14} />}
              onClick={reset}>
              Try again
            </Button>
          )}
          {/* A document navigation, not a router one: leaving an error
              boundary should discard the state that produced the error, and a
              client-side transition would carry it along. */}
          <Button size="xs" variant="subtle" color="slate" component="a" href="/">
            Back to Home
          </Button>
        </Group>
      </Stack>
    </Box>
  )
}
