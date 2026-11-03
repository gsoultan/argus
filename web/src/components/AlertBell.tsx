import { lazy, Suspense, useEffect, useRef, useState } from 'react'
import {
  ActionIcon, Box, Button, Center, Drawer, Group, Indicator, Loader, Popover, Text, Tooltip,
} from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { notifications } from '@mantine/notifications'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import { useRouter } from '@tanstack/react-router'
import { IconBell } from '@tabler/icons-react'
import {
  alertPrefsQuery, chime, notifyDesktop, subscribeAlertStream, unreadAlertsQuery, urgent,
  type ConsoleAlert, type StreamStatus,
} from '~/lib/alerts'
import { FS } from '~/theme'

// The list itself is only fetched when the bell is opened.
const AlertPanel = lazy(() => import('~/components/AlertPanel'))

const TONE: Record<ConsoleAlert['severity'], string> = { info: 'azure', warning: 'amber', critical: 'rose' }

/**
 * The bell in the console header: an unread count, and the alert list.
 *
 * On a phone the list opens full screen instead of as a dropdown: a 420px
 * panel hanging off a 390px screen is unreadable, and a full-screen panel has
 * room for buttons a thumb can hit.
 */
export function AlertBell() {
  const [open, setOpen] = useState(false)
  const phone = useMediaQuery('(max-width: 40em)')
  const { data } = useQuery(unreadAlertsQuery())
  const status = useAlertStream(open)
  const unread = data?.unread ?? 0
  const label = unread === 0 ? 'Alerts' : `Alerts, ${unread >= 100 ? 'more than 99' : unread} unread`

  const bell = (
    <Indicator
      label={unread >= 100 ? '99+' : unread}
      disabled={unread === 0}
      color="rose"
      size={17}
      offset={6}
      withBorder
      styles={{ indicator: { fontSize: 10, fontWeight: 700, padding: '0 4px' } }}
    >
      <ActionIcon
        variant={open ? 'light' : 'subtle'}
        color="slate"
        size="lg"
        aria-label={label}
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => {
          // The list shows what the pop-ups were saying.
          if (!open) notifications.clean()
          setOpen((o) => !o)
        }}
      >
        <IconBell size={19} />
      </ActionIcon>
    </Indicator>
  )

  const panel = (
    <Suspense fallback={<Center h={160}><Loader size="sm" /></Center>}>
      <AlertPanel status={status} onClose={() => setOpen(false)} fullScreen={!!phone} />
    </Suspense>
  )

  if (phone) {
    return (
      <>
        {bell}
        <Drawer
          opened={open}
          onClose={() => setOpen(false)}
          position="right"
          size="100%"
          withCloseButton={false}
          padding={0}
          aria-label="Alerts"
        >
          {open && panel}
        </Drawer>
      </>
    )
  }
  return (
    <Popover
      opened={open}
      onChange={setOpen}
      position="bottom-end"
      width={420}
      shadow="lg"
      radius="md"
      withinPortal
      trapFocus
      returnFocus
    >
      <Popover.Target>
        <Tooltip label={label} disabled={open}>
          <Box>{bell}</Box>
        </Tooltip>
      </Popover.Target>
      <Popover.Dropdown p={0} aria-label="Alerts">
        {open && panel}
      </Popover.Dropdown>
    </Popover>
  )
}

/**
 * Keeps the bell's queries current from the stream, and announces what is
 * new: a pop-up while the console is looked at, a desktop notification when
 * it is not and this browser was allowed to, a chime when asked for.
 */
function useAlertStream(panelOpen: boolean): StreamStatus {
  const qc = useQueryClient()
  const router = useRouter()
  const { data: prefs } = useQuery(alertPrefsQuery())
  const [status, setStatus] = useState<StreamStatus>('connecting')
  // Read through refs so a preference change does not reopen the stream.
  const prefsRef = useRef(prefs)
  prefsRef.current = prefs
  const panelRef = useRef(panelOpen)
  panelRef.current = panelOpen

  useEffect(() => {
    const go = (link: string) => router.history.push(link)
    return subscribeAlertStream(
      (e) => {
        if (e.type === 'end') return
        void qc.invalidateQueries({ queryKey: ['alerts', 'list'] })
        if (e.type === 'read') void qc.invalidateQueries({ queryKey: ['alerts', 'prefs'] })
        if (e.type !== 'alert') return
        const a = e.alert
        const p = prefsRef.current
        notifyDesktop(a, go)
        if (document.visibilityState !== 'visible' || panelRef.current) return
        if (p?.sound && urgent(a)) chime()
        if (p && !p.popup) return
        notifications.show({
          id: `alert-${a.seq}`,
          color: TONE[a.severity],
          title: a.title,
          // What must be acted on stays until dismissed; the rest goes.
          autoClose: urgent(a) ? false : 8000,
          message: (
            <Box>
              {a.detail && <Text size={FS.meta} c="dimmed" lineClamp={2}>{a.detail}</Text>}
              {a.link && (
                <Group gap="xs" mt={6}>
                  <Button
                    size="compact-xs"
                    onClick={() => {
                      notifications.hide(`alert-${a.seq}`)
                      go(a.link)
                    }}
                  >
                    {a.kind === 'approval' ? 'Review' : 'Open'}
                  </Button>
                </Group>
              )}
            </Box>
          ),
        })
      },
      setStatus,
    )
  }, [qc, router])

  return status
}
