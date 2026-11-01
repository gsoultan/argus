import { useEffect } from 'react'
import { Button, Group, Text } from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconDownload } from '@tabler/icons-react'
import { useRegisterSW } from 'virtual:pwa-register/react'
import { busyWith } from '~/lib/update'

const ID = 'argus-update'

/**
 * Puts a new build in place without dropping anything someone has open.
 *
 * The worker was registered with `autoUpdate`, which reloads whenever a new
 * version is precached. In this console that can land while someone is watching
 * a live session or part-way through writing a termination reason — the socket
 * drops and the text is gone, with no explanation and nothing to blame.
 *
 * Then it only offered, and that meant nobody upgraded: see lib/update.ts. Now
 * it reloads at once when nothing is open, and otherwise offers, and goes in by
 * itself as soon as they leave the page that was holding it.
 */
export function PWAUpdate() {
  const {
    needRefresh: [needRefresh],
    updateServiceWorker,
  } = useRegisterSW({
    onRegisterError(err) {
      // A worker that fails to register is not worth interrupting anyone over:
      // the console works without it, only without offline caching.
      console.warn('argus: service worker registration failed', err)
    },
  })

  useEffect(() => {
    if (!needRefresh) return
    const busy = () =>
      busyWith(window.location.pathname, document.querySelector('[role="dialog"]') !== null)
    if (!busy()) {
      void updateServiceWorker(true)
      return
    }
    // Navigation here is pushState, which fires no event, so look again shortly.
    const timer = window.setInterval(() => {
      if (!busy()) void updateServiceWorker(true)
    }, 3_000)
    notifications.show({
      id: ID,
      color: 'sky',
      title: 'A new version of Argus is ready',
      autoClose: false,
      withCloseButton: true,
      message: (
        <Group gap="xs" mt={6} wrap="nowrap">
          <Text size="xs" c="dimmed" style={{ flex: 1 }}>
            Reloading will close anything open — a live terminal, a session you are watching.
          </Text>
          <Button
            size="compact-xs"
            color="sky"
            leftSection={<IconDownload size={12} />}
            onClick={() => void updateServiceWorker(true)}
          >
            Reload
          </Button>
        </Group>
      ),
    })
    return () => window.clearInterval(timer)
  }, [needRefresh, updateServiceWorker])

  return null
}
