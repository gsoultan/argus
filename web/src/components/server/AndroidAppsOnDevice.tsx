import { useMemo, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Button, Group, Menu, Modal, SegmentedControl, Select, Stack, Table, Text, TextInput,
} from '@mantine/core'
import { useQuery, queryOptions } from '@tanstack/react-query'
import { IconApps, IconDots, IconDownload, IconSearch } from '@tabler/icons-react'
import { EmptyState, SectionCard } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import { RecentChanges } from '~/components/server/InstalledSoftware'
import { systemQuery } from '~/components/server/SystemCard'
import { isConfigured, live } from '~/lib/live'
import { run as notifyRun } from '~/lib/notify'
import { runFinished } from '~/lib/packageApi'
import { signerShort, sizeOf, softwareRunText, type AppAction, type SoftwareFile } from '~/lib/softwareApi'
import { softwareFilesQuery, softwareRunsQuery, useAndroidAppRun } from '~/lib/softwareQueries'
import { FS, SP } from '~/theme'
import type { Asset, ServerAndroidApp, ServerAndroidApps } from '~/types/domain'

/**
 * What is installed on a device, as its app read it from the package manager:
 * the package id, the label the launcher shows, the version, and where the
 * app came from (Play, preinstalled, sideloaded). Names and versions only --
 * nothing a secret lives in.
 *
 * This is a device's "what runs here", the counterpart of the databases and
 * brokers a server reports. The app-install work hangs its per-row actions
 * and a top-right button off this list later, so the layout leaves room.
 */
export const androidAppsQuery = (asset: Asset) =>
  queryOptions({
    queryKey: ['asset', asset.id, 'android-apps'] as const,
    queryFn: () => live.orFallback(() => live.androidApps(asset.id), async () => fixtureAndroidApps(asset)),
  })

/** The source filter: all, or one origin. */
type SourceFilter = 'all' | 'play' | 'preinstalled' | 'sideloaded'

const SOURCE_LABEL: Record<string, string> = {
  play: 'Play',
  preinstalled: 'Preinstalled',
  sideloaded: 'Sideloaded',
}

/**
 * Apps the console never offers to uninstall or disable, as the control plane
 * refuses them too: Argus itself, and what a device cannot be used without.
 */
export const PROTECTED_APPS: Record<string, string> = {
  'com.gsoultan.argus': 'how Argus reaches this device',
  'com.android.settings': "the device's Settings",
  'com.android.systemui': "the device's System UI",
  'com.android.vending': 'the Play Store',
  'com.google.android.gms': 'Google Play services',
  'com.android.packageinstaller': "Android's package installer",
  'com.google.android.packageinstaller': "Android's package installer",
  'com.android.phone': 'the phone service',
  android: 'Android itself',
}

const isAppRun = (mode: string) => mode.startsWith('app_')

/** What the person picking an action is told before it is sent. */
const CONFIRM: Record<Exclude<AppAction, 'install'>, { title: string; button: string; body: (attended: boolean) => string }> = {
  uninstall: {
    title: 'Uninstall', button: 'Uninstall',
    body: (attended) => `The app and its data are removed from the device. ${attended
      ? 'Android asks the person holding the device to confirm; they can say no.'
      : 'The helper removes it; nobody on the device is asked.'} Reinstalling it later starts it fresh.`,
  },
  disable: {
    title: 'Disable', button: 'Disable',
    body: () => "A preinstalled app cannot be removed from the system image; this takes it away from the device's user. Its files stay, and Restore brings it back.",
  },
  restore: {
    title: 'Restore', button: 'Restore',
    body: () => "The preinstalled app comes back for the device's user, at the version in the system image.",
  },
}

export function AndroidAppsOnDevice({ asset, canEdit = false }: { asset: Asset; canEdit?: boolean }) {
  const { data } = useQuery(androidAppsQuery(asset))
  const { data: sys } = useQuery(systemQuery(asset))
  const live_ = isConfigured()
  const { data: runs } = useQuery(softwareRunsQuery(asset.id, live_ && !!data?.reported))
  const [filter, setFilter] = useState<SourceFilter>('all')
  const [q, setQ] = useState('')
  const [installing, setInstalling] = useState(false)
  const [confirm, setConfirm] = useState<{ action: Exclude<AppAction, 'install'>; app: ServerAndroidApp } | null>(null)

  const engine = sys?.system?.engine ?? 'attended'
  const attended = engine !== 'unattended'
  const canInstall = sys?.system?.canInstallApps
  const appRuns = (runs?.runs ?? []).filter((r) => isAppRun(r.mode))
  const going = appRuns.find((r) => !runFinished(r.state))
  const editable = canEdit && live_ && !!data?.reported

  const shown = useMemo(() => {
    const apps = data?.apps ?? []
    const needle = q.trim().toLowerCase()
    return apps.filter((a) => {
      if (filter !== 'all' && a.source !== filter) return false
      if (!needle) return true
      return a.label.toLowerCase().includes(needle) || a.package.toLowerCase().includes(needle)
    })
  }, [data, filter, q])

  return (
    <SectionCard
      title="What runs here"
      icon={IconApps}
      description={data?.reported
        ? <>Installed on the device {data.checkedAt ? <span title={absTime(data.checkedAt)}>{relTime(data.checkedAt)}</span> : 'recently'}: package names, labels and versions only.</>
        : undefined}
      badge={data?.reported ? <Badge size="xs" variant="light" color="slate">{data.apps.length}</Badge> : undefined}
      action={editable ? (
        <Button size="compact-sm" leftSection={<IconDownload size={14} />} onClick={() => setInstalling(true)}
          disabled={!!going || canInstall === false} title={going ? 'One change at a time: wait for this one to end' : undefined}>
          Install app…
        </Button>
      ) : undefined}
    >
      {editable && canInstall === false && (
        <Alert color="amber" mb="sm" title="This device does not let Argus install apps yet">
          <Text size={FS.body}>
            Open Argus on the device and tap <b>Allow installing apps</b>; Android asks once. Uninstalling works without it.
          </Text>
        </Alert>
      )}
      {going && (
        <Alert color="sky" mb="sm" title={`${going.requestedBy} asked to ${softwareRunText(going)}`}>
          <Text size={FS.body}>
            {going.state === 'awaiting_approval' ? 'Waiting for approval; approve it below or in Activity.'
              : going.message || (going.sentAt ? 'The device is working on it.' : 'Sent at the device’s next check-in, within a minute.')}
          </Text>
        </Alert>
      )}
      {!data ? null : !data.reported ? (
        <EmptyState compact icon={IconApps} title="Not reported yet."
          description={asset.agentState === 'absent'
            ? 'This device has no app connected. Once the Argus app links and checks in, the installed apps appear here within a few minutes.'
            : 'The Argus app on this device has not reported its installed apps. An app older than this console does not list them; updating it is enough.'} />
      ) : (
        <>
          <Group justify="space-between" mb="sm" gap={SP.snug} wrap="wrap">
            <SegmentedControl
              size="xs"
              value={filter}
              onChange={(v) => setFilter(v as SourceFilter)}
              data={[
                { label: 'All', value: 'all' },
                { label: 'Play', value: 'play' },
                { label: 'Preinstalled', value: 'preinstalled' },
                { label: 'Sideloaded', value: 'sideloaded' },
              ]}
            />
            <TextInput
              size="xs"
              w={220}
              placeholder="Filter apps"
              leftSection={<IconSearch size={14} />}
              value={q}
              onChange={(e) => setQ(e.currentTarget.value)}
            />
          </Group>
          {shown.length === 0 ? (
            <EmptyState compact icon={IconApps} title="No apps match." description="Clear the filter to see them all." />
          ) : (
            <Table.ScrollContainer minWidth={0} type="native">
              <Table verticalSpacing="xs">
                <Table.Tbody>
                  {shown.map((a) => (
                    <AppRow key={a.package} app={a} actions={editable && !going}
                      unattended={!attended} onAction={(action) => setConfirm({ action, app: a })} />
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )}
          {appRuns.length > 0 && (
            <Stack mt="md">
              <RecentChanges assetId={asset.id} runs={appRuns} canEdit={canEdit && live_} onPreview={() => {}} />
            </Stack>
          )}
        </>
      )}
      {installing && (
        <InstallApp asset={asset} attended={attended} installed={data?.apps ?? []} onClose={() => setInstalling(false)} />
      )}
      {confirm && (
        <ConfirmApp asset={asset} attended={attended} {...confirm} onClose={() => setConfirm(null)} />
      )}
    </SectionCard>
  )
}

/** Picks a library APK and sends it to the device. */
function InstallApp({ asset, attended, installed, onClose }: {
  asset: Asset; attended: boolean; installed: ServerAndroidApp[]; onClose: () => void
}) {
  const { data: files, error } = useQuery(softwareFilesQuery('android'))
  const run = useAndroidAppRun(asset.id)
  const [id, setId] = useState<string | null>(null)
  const file: SoftwareFile | undefined = files?.find((f) => f.id === id)
  const current = file?.app && installed.find((a) => a.package === file.app!.package)
  const send = () => file && void notifyRun(async () => {
    await run.mutateAsync({ action: 'install', fileId: file.id })
    onClose()
  }, {
    failure: 'Not sent',
    success: ['Sent to the device', attended ? 'The person holding it is asked to confirm.' : 'The helper installs it.'],
  })
  return (
    <Modal opened onClose={onClose} title={`Install an app on ${asset.hostname}`} size="lg">
      <Stack gap={SP.cozy}>
        {error ? (
          <Alert color="rose"><Text size={FS.body}>{error instanceof Error ? error.message : String(error)}</Text></Alert>
        ) : files && files.length === 0 ? (
          <EmptyState compact icon={IconApps} title="No APKs in the library."
            description="Add one in Settings, Software, Apps for devices. Each is read when it is added, so the device can check it is the same file." />
        ) : (
          <Select label="App" placeholder="Pick an APK from the library" searchable value={id} onChange={setId}
            data={(files ?? []).map((f) => ({ value: f.id, label: `${f.name} ${f.app?.versionName ?? ''}`.trim() }))} />
        )}
        {file?.app && (
          <Alert color="slate" variant="light">
            <Text size={FS.body} ff="monospace">{file.app.package}</Text>
            <Text size={FS.meta}>
              Version {file.app.versionName} (code {file.app.versionCode}) · {sizeOf(file.size)} · signed {file.app.scheme} by{' '}
              <Text span ff="monospace" title={file.app.signer}>{signerShort(file.app.signer)}</Text>
            </Text>
            <Text size={FS.meta} mt={4}>
              {current
                ? `${current.label || current.package} ${current.version} is installed; this updates it. Android refuses a file signed by a different key, and an older version.`
                : 'Not on the device yet.'}
            </Text>
          </Alert>
        )}
        <Text size={FS.meta} c="dimmed">
          The device downloads the file over its own connection and checks it is this file, this version and this signer before Android installs it.{' '}
          {attended ? 'Android asks the person holding the device to confirm; they can say no, and nothing changes.' : 'The helper installs it; nobody on the device is asked.'}
        </Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!file} loading={run.isPending} onClick={send}>Install</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function ConfirmApp({ asset, attended, action, app, onClose }: {
  asset: Asset; attended: boolean; action: Exclude<AppAction, 'install'>; app: ServerAndroidApp; onClose: () => void
}) {
  const run = useAndroidAppRun(asset.id)
  const c = CONFIRM[action]
  const name = app.label || app.package
  const send = () => void notifyRun(async () => {
    await run.mutateAsync({ action, package: app.package })
    onClose()
  }, { failure: 'Not sent', success: ['Sent to the device', `${c.title} ${name} at its next check-in.`] })
  return (
    <Modal opened onClose={onClose} title={`${c.title} ${name}?`}>
      <Stack gap={SP.cozy}>
        <Text size={FS.meta} c="dimmed" ff="monospace">{app.package} {app.version}</Text>
        <Text size={FS.body}>{c.body(attended)}</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color={action === 'restore' ? undefined : 'rose'} loading={run.isPending} onClick={send}>{c.button}</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function AppRow({ app, actions, unattended, onAction }: {
  app: ServerAndroidApp; actions: boolean; unattended: boolean; onAction: (a: Exclude<AppAction, 'install'>) => void
}) {
  const guarded = PROTECTED_APPS[app.package]
  const pre = app.source === 'preinstalled'
  // A preinstalled app is disabled, not uninstalled, and only the helper can.
  const items: Exclude<AppAction, 'install'>[] = guarded ? [] : pre ? (unattended ? [app.disabled ? 'restore' : 'disable'] : []) : ['uninstall']
  return (
    <Table.Tr>
      <Table.Td>
        <Text size={FS.body} fw={600}>{app.label || app.package}</Text>
        <Text size={FS.meta} c="dimmed" ff="monospace">{app.package}</Text>
      </Table.Td>
      <Table.Td style={{ whiteSpace: 'nowrap' }}>
        {app.version && <Text size={FS.meta} c="dimmed">{app.version}</Text>}
      </Table.Td>
      <Table.Td style={{ whiteSpace: 'nowrap' }}>
        {app.source && (
          <Badge size="xs" variant="light" color={app.source === 'sideloaded' ? 'amber' : 'slate'}>
            {SOURCE_LABEL[app.source] ?? app.source}
          </Badge>
        )}
        {app.disabled && <Badge size="xs" variant="outline" color="slate" ml={4}>Disabled</Badge>}
      </Table.Td>
      <Table.Td style={{ width: 1, whiteSpace: 'nowrap' }}>
        {actions && (guarded ? (
          <Text size={FS.micro} c="dimmed" title={`Not changed from the console: ${guarded}`}>Kept</Text>
        ) : items.length > 0 && (
          <Menu position="bottom-end" withinPortal>
            <Menu.Target>
              <ActionIcon variant="subtle" color="slate" aria-label={`Actions for ${app.label || app.package}`}><IconDots size={14} /></ActionIcon>
            </Menu.Target>
            <Menu.Dropdown>
              {items.map((a) => (
                <Menu.Item key={a} color={a === 'restore' ? undefined : 'rose'} onClick={() => onAction(a)}>{CONFIRM[a].title}…</Menu.Item>
              ))}
            </Menu.Dropdown>
          </Menu>
        ))}
      </Table.Td>
    </Table.Tr>
  )
}

/** A few apps so the dev build's device has a list without a phone. */
function fixtureAndroidApps(a: Asset): ServerAndroidApps {
  if (a.kind !== 'android' || a.agentState === 'absent') {
    return { reported: false, checkedAt: null, apps: [] }
  }
  return {
    reported: true,
    checkedAt: a.agentLastSeenAt ?? null,
    apps: [
      { package: 'com.android.chrome', label: 'Chrome', version: '131.0.6778.81', source: 'play' },
      { package: 'com.google.android.gm', label: 'Gmail', version: '2024.10.20', source: 'play' },
      { package: 'com.android.settings', label: 'Settings', version: '15', source: 'preinstalled' },
      { package: 'com.android.dialer', label: 'Phone', version: '15', source: 'preinstalled' },
      { package: 'com.android.calculator2', label: 'Calculator', version: '15', source: 'preinstalled', disabled: true },
      { package: 'id.northwind.field', label: 'Northwind Field', version: '3.2.0', source: 'sideloaded' },
      { package: 'com.gsoultan.argus', label: 'Argus', version: '1.0.0', source: 'sideloaded' },
    ],
  }
}
