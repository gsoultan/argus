import { useMemo, useState } from 'react'
import { Badge, Group, SegmentedControl, Table, Text, TextInput } from '@mantine/core'
import { useQuery, queryOptions } from '@tanstack/react-query'
import { IconApps, IconSearch } from '@tabler/icons-react'
import { EmptyState, SectionCard } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import { live } from '~/lib/live'
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

export function AndroidAppsOnDevice({ asset }: { asset: Asset }) {
  const { data } = useQuery(androidAppsQuery(asset))
  const [filter, setFilter] = useState<SourceFilter>('all')
  const [q, setQ] = useState('')

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
    >
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
                    <AppRow key={a.package} app={a} />
                  ))}
                </Table.Tbody>
              </Table>
            </Table.ScrollContainer>
          )}
        </>
      )}
    </SectionCard>
  )
}

function AppRow({ app }: { app: ServerAndroidApp }) {
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
      </Table.Td>
      {/* The app-install work fills this action column per row. */}
      <Table.Td style={{ width: 1, whiteSpace: 'nowrap' }} />
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
      { package: 'id.northwind.field', label: 'Northwind Field', version: '3.2.0', source: 'sideloaded' },
      { package: 'com.gsoultan.argus', label: 'Argus', version: '1.0.0', source: 'sideloaded' },
    ],
  }
}
