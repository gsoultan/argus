import { useCallback, useEffect, useRef, useState } from 'react'
import { Button, Group, Text } from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconDownload } from '@tabler/icons-react'
import { useRegisterSW } from 'virtual:pwa-register/react'
import { CHECK_BUILD_EVENT, busyWith, isNewBuild, runningEntry, servedEntry } from '~/lib/update'

const ID = 'argus-update'
const TRIED = 'argus-update-tried'

/** Whether this tab has not yet reloaded for this build. Records that it has. */
function firstTryFor(entry: string): boolean {
  try {
    if (sessionStorage.getItem(TRIED) === entry) return false
    sessionStorage.setItem(TRIED, entry)
  } catch {
    // Storage refused (a private window): reload; the worst case is the offer.
  }
  return true
}

/** How often an open console asks whether the server has a new build. */
const CHECK_EVERY_MS = 60_000

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
 *
 * And it looks for the new build itself. The worker only checks on a page
 * load, which a single-page console left open across an update never has, so
 * the update finished and the tab went on running the old one. Now the console
 * asks the server every minute, when the tab comes back into view, and when an
 * update finishes; see "Noticing" in lib/update.ts.
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
  // A new build with no worker to bring it in: no secure context (plain http
  // on a LAN address), or one that failed to register. A reload is enough
  // then, because the server sends index.html with no-cache.
  const [staleWithoutWorker, setStaleWithoutWorker] = useState(false)
  const [autoReload, setAutoReload] = useState(true)

  const checking = useRef(false)
  const found = useRef(false)
  const check = useCallback(async () => {
    if (checking.current || found.current || document.visibilityState === 'hidden') return
    checking.current = true
    try {
      const served = await servedEntry()
      if (!isNewBuild(runningEntry(), served)) return
      const reg = await navigator.serviceWorker?.getRegistration().catch(() => undefined)
      if (reg) {
        // Fetches the new sw.js; once it installs, needRefresh follows.
        await reg.update().catch(() => undefined)
        if (reg.installing || reg.waiting) return
        // The worker would not move to the new build. Without it, the reload
        // below goes to the server for index.html instead of the worker's copy
        // of the old one, and the new build registers its own.
        await reg.unregister().catch(() => undefined)
      }
      // Once per build: if a reload somehow still came back to the old one,
      // offer rather than reload again and again.
      found.current = true
      setAutoReload(firstTryFor(served))
      setStaleWithoutWorker(true)
    } finally {
      checking.current = false
    }
  }, [])

  useEffect(() => {
    void check()
    const timer = window.setInterval(() => void check(), CHECK_EVERY_MS)
    const onVisible = () => { if (document.visibilityState === 'visible') void check() }
    const onCheck = () => void check()
    // A route chunk that no longer exists is the surest sign of all.
    const onPreloadError = () => void check()
    document.addEventListener('visibilitychange', onVisible)
    window.addEventListener(CHECK_BUILD_EVENT, onCheck)
    window.addEventListener('vite:preloadError', onPreloadError)
    return () => {
      window.clearInterval(timer)
      document.removeEventListener('visibilitychange', onVisible)
      window.removeEventListener(CHECK_BUILD_EVENT, onCheck)
      window.removeEventListener('vite:preloadError', onPreloadError)
    }
  }, [check])

  const ready = needRefresh || staleWithoutWorker
  useEffect(() => {
    if (!ready) return
    const apply = () => {
      if (needRefresh) void updateServiceWorker(true)
      else window.location.reload()
    }
    const busy = () =>
      busyWith(window.location.pathname, document.querySelector('[role="dialog"]') !== null)
    if (!busy() && (needRefresh || autoReload)) {
      apply()
      return
    }
    // Navigation here is pushState, which fires no event, so look again shortly.
    const timer = window.setInterval(() => {
      if (!busy() && (needRefresh || autoReload)) apply()
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
            onClick={apply}
          >
            Reload
          </Button>
        </Group>
      ),
    })
    return () => window.clearInterval(timer)
  }, [ready, needRefresh, autoReload, updateServiceWorker])

  return null
}
