import { Box, Text } from '@mantine/core'
import { createFileRoute } from '@tanstack/react-router'
import { ButtonLink } from '~/components/links'
import { FS } from '~/theme'

/**
 * /setup for someone already signed in.
 *
 * Signed out, LoginGate draws the setup page itself. Signed in, an account
 * exists by definition, so setup is closed and this says so rather than
 * falling through to a 404 that reads like the page was never there.
 */
export const Route = createFileRoute('/setup')({
  component: SetupDone,
})

function SetupDone() {
  return (
    <Box className="grid place-items-center" h="60vh">
      <Box ta="center" maw={420}>
        <Text size={FS.title} fw={700}>Setup is complete</Text>
        <Text size="sm" c="dimmed" mt="xs" mb="md">
          This control plane has accounts, so first-run setup is closed. Further
          accounts are added on the host with argus-control users add.
        </Text>
        <ButtonLink size="xs" variant="light" to="/">
          Back to overview
        </ButtonLink>
      </Box>
    </Box>
  )
}
