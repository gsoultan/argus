import { useState, type ReactNode } from 'react'
import {
  ActionIcon, Badge, Box, Drawer, Button, Center, Divider, Group, Loader, ScrollArea, SegmentedControl,
  Stack, Switch, Text, ThemeIcon, UnstyledButton,
} from '@mantine/core'
import { useInfiniteQuery, useQuery } from '@tanstack/react-query'
import { useRouter } from '@tanstack/react-router'
import {
  IconAlertTriangle, IconArrowLeft, IconDatabase, IconHourglass, IconLogin2, IconServer, IconSettings,
  IconTerminal2, IconX,
} from '@tabler/icons-react'
import {
  alertPrefsQuery, unreadAlertsQuery, type AlertKind, type AlertPrefs, type ConsoleAlert, type StreamStatus,
} from '~/lib/alerts'
import {
  ALERT_KINDS, allAlertsQuery, desktopPermission, desktopWanted, setDesktopWanted, useMarkAlertsRead,
  useSaveAlertPrefs,
} from '~/lib/alertsPanel'
import { meQuery } from '~/lib/queries'
import { seesWholeFleet } from '~/lib/roles'
import { notifyError } from '~/lib/notify'
import { FS, SP } from '~/theme'

const KIND_ICON: Record<AlertKind, typeof IconHourglass> = {
  approval: IconHourglass,
  signin: IconLogin2,
  session: IconTerminal2,
  security: IconAlertTriangle,
  server: IconServer,
  backup: IconDatabase,
}

const TONE: Record<ConsoleAlert['severity'], string> = { info: 'slate', warning: 'amber', critical: 'rose' }

const STATUS: Record<StreamStatus, { label: string; color: string; hint: string }> = {
  live: { label: 'Live', color: 'var(--mantine-color-teal-6)', hint: 'New alerts arrive as they happen.' },
  connecting: { label: 'Connecting…', color: 'var(--mantine-color-slate-5)', hint: 'Opening the live connection.' },
  reconnecting: { label: 'Reconnecting…', color: 'var(--mantine-color-amber-6)',
    hint: 'The live connection dropped. Anything missed is filled in when it is back.' },
  ended: { label: 'Signed out', color: 'var(--mantine-color-rose-6)', hint: 'Sign in again to keep receiving alerts.' },
  fixture: { label: 'Sample data', color: 'var(--mantine-color-amber-6)',
    hint: 'Not connected to Argus: these alerts are invented.' },
}

/** How long ago, in the words a person would use. */
export function agoText(iso: string, now = Date.now()): string {
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000))
  if (s < 60) return 'just now'
  const m = Math.round(s / 60)
  if (m < 60) return `${m} min ago`
  const h = Math.round(m / 60)
  if (h < 24) return `${h} h ago`
  const d = Math.round(h / 24)
  return d === 1 ? 'yesterday' : `${d} days ago`
}

/** The bell's list on a phone: the whole screen, with room for a thumb. */
export function PhoneAlerts({ onClose, children }: { onClose: () => void; children: ReactNode }) {
  return (
    <Drawer opened onClose={onClose} position="right" size="100%" withCloseButton={false} padding={0}
      aria-label="Alerts">
      {children}
    </Drawer>
  )
}

/** The list under the bell, and the person's choices for it. */
export default function AlertPanel({ status, onClose, fullScreen }: {
  status: StreamStatus
  onClose: () => void
  fullScreen: boolean
}) {
  const [view, setView] = useState<'list' | 'prefs'>('list')
  const [tab, setTab] = useState<'unread' | 'all'>('unread')
  const unread = useQuery(unreadAlertsQuery())
  const all = useInfiniteQuery({ ...allAlertsQuery(), enabled: tab === 'all' })
  const mark = useMarkAlertsRead()
  const router = useRouter()
  const st = STATUS[status]

  const alerts = tab === 'unread'
    ? unread.data?.alerts ?? []
    : (all.data?.pages.flatMap((p) => p.alerts) ?? [])
  const loading = tab === 'unread' ? unread.isPending : all.isPending
  const failed = tab === 'unread' ? unread.error : all.error
  const newest = Math.max(0, ...(unread.data?.alerts.map((a) => a.seq) ?? []))
  const count = unread.data?.unread ?? 0

  const open = (a: ConsoleAlert) => {
    if (!a.read) mark.mutate({ seqs: [a.seq] })
    if (a.link) {
      onClose()
      router.history.push(a.link)
    }
  }

  const header = (
    <Group justify="space-between" px="md" pt="sm" pb={SP.cozy} wrap="nowrap" gap="xs">
      <Group gap="xs" wrap="nowrap">
        {view === 'prefs' && (
          <ActionIcon variant="subtle" color="slate" aria-label="Back to alerts" onClick={() => setView('list')}>
            <IconArrowLeft size={16} />
          </ActionIcon>
        )}
        <Text fw={650} size="md">{view === 'prefs' ? 'Alert settings' : 'Alerts'}</Text>
        {view === 'list' && (
          <Group gap={SP.tight} wrap="nowrap" title={st.hint}>
            <Box w={7} h={7} style={{ borderRadius: '50%', background: st.color }} />
            <Text size={FS.micro} c="dimmed">{st.label}</Text>
          </Group>
        )}
      </Group>
      <Group gap={SP.tight} wrap="nowrap">
        {view === 'list' && count > 0 && (
          <Button
            variant="subtle"
            size="compact-xs"
            loading={mark.isPending}
            onClick={() => mark.mutate({ upTo: newest }, { onError: (e) => notifyError('Not marked read', e) })}
          >
            Mark all read
          </Button>
        )}
        {fullScreen && (
          <ActionIcon variant="subtle" color="slate" aria-label="Close alerts" onClick={onClose}>
            <IconX size={18} />
          </ActionIcon>
        )}
      </Group>
    </Group>
  )

  if (view === 'prefs') {
    return (
      <Box>
        {header}
        <Divider />
        <ScrollArea.Autosize mah={fullScreen ? 'calc(100dvh - 56px)' : '70vh'}>
          <AlertPreferences />
        </ScrollArea.Autosize>
      </Box>
    )
  }

  return (
    <Box style={fullScreen ? { display: 'flex', flexDirection: 'column', height: '100dvh' } : undefined}>
      {header}
      <Box px="md" pb={SP.cozy}>
        <SegmentedControl
          size="xs"
          value={tab}
          onChange={(v) => setTab(v as 'unread' | 'all')}
          data={[{ value: 'unread', label: count ? `Unread ${count >= 100 ? '99+' : count}` : 'Unread' },
            { value: 'all', label: 'All' }]}
        />
      </Box>
      <Divider />
      <ScrollArea.Autosize mah={fullScreen ? undefined : 'min(70vh, 560px)'} style={fullScreen ? { flex: 1 } : undefined}>
        {loading ? (
          <Center h={140}><Loader size="sm" /></Center>
        ) : failed ? (
          <Text size={FS.body} c="rose" p="md">Could not load alerts: {failed.message}</Text>
        ) : alerts.length === 0 ? (
          <Stack align="center" gap={SP.tight} py="xl" px="md">
            <Text size={FS.body} fw={600}>{tab === 'unread' ? 'You are all caught up' : 'No alerts yet'}</Text>
            <Text size={FS.meta} c="dimmed" ta="center">
              Approval requests, sign-ins and other events you can see appear here as they happen.
            </Text>
          </Stack>
        ) : (
          <Stack gap={0}>
            {alerts.map((a) => <AlertRow key={a.seq} alert={a} onOpen={() => open(a)} />)}
            {tab === 'all' && all.hasNextPage && (
              <Center py="sm">
                <Button variant="subtle" size="compact-sm" loading={all.isFetchingNextPage}
                  onClick={() => void all.fetchNextPage()}>
                  Show older
                </Button>
              </Center>
            )}
          </Stack>
        )}
      </ScrollArea.Autosize>
      <Divider />
      <Group justify="space-between" px="md" py={SP.cozy}>
        <Button variant="subtle" size="compact-sm" leftSection={<IconSettings size={14} />} onClick={() => setView('prefs')}>
          Alert settings
        </Button>
        <Text size={FS.micro} c="dimmed">Kept for 30 days</Text>
      </Group>
    </Box>
  )
}

function AlertRow({ alert: a, onOpen }: { alert: ConsoleAlert; onOpen: () => void }) {
  const Icon = KIND_ICON[a.kind] ?? IconAlertTriangle
  const tone = a.kind === 'session' && a.severity === 'info' ? 'sky' : TONE[a.severity]
  return (
    <UnstyledButton
      onClick={onOpen}
      px="md"
      py="sm"
      aria-label={`${a.read ? '' : 'Unread: '}${a.title}`}
      style={{
        display: 'flex', gap: 12, borderBottom: '1px solid var(--color-line)', position: 'relative',
        background: a.read ? undefined : 'var(--mantine-color-azure-light)',
      }}
    >
      {!a.read && (
        <Box pos="absolute" left={6} top={22} w={6} h={6}
          style={{ borderRadius: '50%', background: 'var(--mantine-color-azure-6)' }} />
      )}
      <ThemeIcon variant="light" color={tone} size={32} radius="md" style={{ flexShrink: 0 }}>
        <Icon size={16} />
      </ThemeIcon>
      <Box style={{ flex: 1, minWidth: 0 }}>
        <Text size={FS.body} fw={a.read ? 500 : 600} lh={1.35}>{a.title}</Text>
        {a.detail && <Text size={FS.meta} c="dimmed" lh={1.4} mt={SP.hair} lineClamp={3}>{a.detail}</Text>}
        <Group gap="xs" mt={SP.snug}>
          <Text size={FS.micro} c="dimmed">{agoText(a.at)}</Text>
          {a.kind === 'approval' && a.severity !== 'info' && a.link && (
            <Badge size="xs" variant="light" color="azure">Review</Badge>
          )}
        </Group>
      </Box>
    </UnstyledButton>
  )
}

/**
 * The person's own choices for their bell. Saved on their account, so they
 * follow them to another browser; except desktop notifications, which each
 * browser asks for itself.
 */
export function AlertPreferences() {
  const { data: prefs } = useQuery(alertPrefsQuery())
  const { data: me } = useQuery(meQuery())
  const save = useSaveAlertPrefs()
  const [desktop, setDesktop] = useState(desktopWanted() && desktopPermission() === 'granted')
  const permission = desktopPermission()
  if (!prefs) return <Center h={140}><Loader size="sm" /></Center>

  const set = (p: Partial<AlertPrefs>) =>
    save.mutate({ ...prefs, ...p }, { onError: (e) => notifyError('Alert settings not saved', e) })
  const muted = new Set(prefs.muted)
  const toggle = (k: AlertKind, on: boolean) => {
    const next = new Set(muted)
    if (on) next.delete(k)
    else next.add(k)
    set({ muted: [...next] })
  }
  // Everyone hears about their own requests; the rest go to whoever reads the
  // whole organization. Kinds a role never receives are not offered.
  const kinds = ALERT_KINDS.filter((k) => k.kind === 'approval' || seesWholeFleet(me?.role))

  const askDesktop = async (on: boolean) => {
    if (!on) {
      setDesktopWanted(false)
      setDesktop(false)
      return
    }
    const got = permission === 'granted' ? 'granted' : await Notification.requestPermission()
    setDesktopWanted(got === 'granted')
    setDesktop(got === 'granted')
    if (got !== 'granted') {
      notifyError('Desktop notifications are blocked',
        new Error('This browser refused. Allow notifications for this site in its settings, then try again.'))
    }
  }

  return (
    <Stack gap={0} pb="xs">
      <Section title="What you're told about" hint="Only for you. Turning a kind off hides it; it is still kept, so turning it back on shows what you missed." />
      {kinds.map((k) => {
        const locked = k.kind === 'security'
        return (
          <Box key={k.kind}>
            <Row
              label={k.label}
              description={k.description}
              control={
                <Switch
                  checked={locked || !muted.has(k.kind)}
                  disabled={locked || save.isPending}
                  onChange={(e) => toggle(k.kind, e.currentTarget.checked)}
                  aria-label={k.label}
                />
              }
              extra={locked ? <Badge size="xs" variant="light" color="slate">Always on</Badge> : null}
            />
            {k.kind === 'signin' && !muted.has('signin') && (
              <Row
                indent
                label="Only new devices"
                description="Skip sign-ins from a browser and address already seen for that account."
                control={
                  <Switch
                    checked={prefs.newDevicesOnly}
                    disabled={save.isPending}
                    onChange={(e) => set({ newDevicesOnly: e.currentTarget.checked })}
                    aria-label="Only new devices"
                  />
                }
              />
            )}
          </Box>
        )
      })}
      <Section title="How you're told" />
      <Row
        label="Pop-up in the console"
        description="Show new alerts at the corner of the screen. Approval requests and security alerts stay until closed."
        control={<Switch checked={prefs.popup} disabled={save.isPending}
          onChange={(e) => set({ popup: e.currentTarget.checked })} aria-label="Pop-up in the console" />}
      />
      <Row
        label="Sound"
        description="A short chime for approval requests and security alerts."
        control={<Switch checked={prefs.sound} disabled={save.isPending}
          onChange={(e) => set({ sound: e.currentTarget.checked })} aria-label="Sound" />}
      />
      <Row
        label="Desktop notifications"
        description={permission === 'unsupported'
          ? 'This browser cannot show notifications.'
          : permission === 'denied'
            ? 'Blocked in this browser. Allow notifications for this site in the browser\'s settings first.'
            : 'When Argus is in a background tab. Set per browser.'}
        control={<Switch checked={desktop} disabled={permission === 'unsupported' || permission === 'denied'}
          onChange={(e) => void askDesktop(e.currentTarget.checked)} aria-label="Desktop notifications" />}
      />
    </Stack>
  )
}

function Section({ title, hint }: { title: string; hint?: string }) {
  return (
    <Box px="md" pt="md" pb={SP.tight}>
      <Text size={FS.meta} fw={700} tt="uppercase" c="dimmed" style={{ letterSpacing: '0.04em' }}>{title}</Text>
      {hint && <Text size={FS.meta} c="dimmed" mt={SP.hair} lh={1.4}>{hint}</Text>}
    </Box>
  )
}

function Row({ label, description, control, extra, indent }: {
  label: string
  description: string
  control: ReactNode
  extra?: ReactNode
  indent?: boolean
}) {
  return (
    <Group wrap="nowrap" align="flex-start" gap="sm" px="md" py={SP.cozy} pl={indent ? 52 : 'md'}>
      <Box pt={2}>{control}</Box>
      <Box style={{ flex: 1, minWidth: 0 }}>
        <Group gap="xs"><Text size={FS.body} fw={600}>{label}</Text>{extra}</Group>
        <Text size={FS.meta} c="dimmed" lh={1.4}>{description}</Text>
      </Box>
    </Group>
  )
}
