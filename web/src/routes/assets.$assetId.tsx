import { Suspense, lazy, useState } from 'react'
import { Badge, Box, Button, Card, Group, Tabs, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconPencil, IconPlugConnected, IconServer2 } from '@tabler/icons-react'
import { ReachBadge, reachOf } from '~/components/myservers'
import { ServerList } from '~/components/ServerList'
import { SplitView } from '~/components/SplitView'
import { EmptyState, PageBody, PageHeader } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { AccessTab } from '~/components/server/AccessTab'
import { AppsTab } from '~/components/server/AppsTab'
import { HostKeyChanged } from '~/components/server/hostKey'
import { OverviewTab } from '~/components/server/OverviewTab'
import { SessionsTab } from '~/components/server/SessionsTab'
import { SettingsTab } from '~/components/server/SettingsTab'
import { UpdatesTab } from '~/components/server/UpdatesTab'
import { cleanable, diskFilling, LogsTab } from '~/components/server/LogsTab'
import { DisksTab, growable, stuck } from '~/components/server/DisksTab'
import { assetDisksQuery } from '~/lib/diskQueries'
import { LOW_FRESH_MS, warningText } from '~/lib/resourcesLow'
import { now } from '~/components/primitives'
import type { Asset } from '~/types/domain'
import { assetLogsQuery } from '~/lib/logQueries'
import { bytes, systemQuery } from '~/components/server/SystemCard'
import { parseServerTab, type ServerTab } from '~/components/server/tabs'
import { TABS_BY_TYPE, tabLabel } from '~/components/server/tabsByType'
import { ASSET_TYPE_LABEL, assetTypeOf } from '~/lib/assetType'
import { AgentTab } from '~/components/server/AgentTab'
import { assetQuery, meQuery, sessionsQuery } from '~/lib/queries'
import { assetPackagesQuery } from '~/lib/packageQueries'
import { isConfigured } from '~/lib/live'
import { guard } from '~/lib/guard'
import { SP } from '~/theme'

/** Opened only by an administrator, so their chunks are fetched only for one. */
const AssetForm = lazy(() =>
  import('~/components/AssetForm').then((m) => ({ default: m.AssetForm })),
)

export const Route = createFileRoute('/assets/$assetId')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  // The tab is in the address, so a link or a reload lands on the same view.
  // Overview is the default and is never written out. An unknown tab is
  // returned as undefined rather than left out: the root route passes the raw
  // query through, so leaving it out would let ?tab=nonsense reach the page.
  validateSearch: (s: Record<string, unknown>): { tab?: ServerTab } => ({ tab: parseServerTab(s.tab) }),
  component: AssetDetail,
  loader: ({ context, params }) =>
    context.queryClient.ensureQueryData(assetQuery(params.assetId)),
})

/**
 * One server, in five tabs: whether it is all right, who can reach it, who
 * did, what runs on it, and the controls it needs rarely. It used to be every
 * one of those at once, as nine cards in two columns.
 */
function AssetDetail() {
  const navigate = useNavigate({ from: Route.fullPath })
  const { assetId } = Route.useParams()
  const asked: ServerTab = parseServerTab(Route.useSearch().tab) ?? 'overview'
  const { data: asset } = useQuery(assetQuery(assetId))
  const { data: allSessions } = useQuery(sessionsQuery())
  const { data: me } = useQuery(meQuery())

  const canEdit = me?.role === 'admin' || me?.role === 'owner'
  const seesUpdates = canEdit || me?.role === 'auditor'
  const { data: pkgs } = useQuery({ ...assetPackagesQuery(assetId), enabled: isConfigured() && seesUpdates && asset?.protocol !== 'rdp' })
  const waiting = pkgs?.report?.updates.length ?? 0
  const securityWaiting = pkgs?.report?.updates.filter((u) => u.security).length ?? 0
  // Logs: read by the same people as updates, on Linux and Windows alike.
  const { data: logs } = useQuery({ ...assetLogsQuery(assetId), enabled: isConfigured() && seesUpdates })
  const freeable = logs?.report ? cleanable(logs) : 0
  const filling = logs ? diskFilling(logs) : false
  const { data: disks } = useQuery({ ...assetDisksQuery(assetId), enabled: isConfigured() && seesUpdates && asset?.kind !== 'android' })
  const growRoom = disks ? growable(disks) : 0
  // Same query as the System card, so the header's word and the card's strip
  // are one answer, fetched once.
  // Old figures never warn: like the assets list, only a recent report
  // from an agent that is still healthy counts.
  const { data: sys } = useQuery({ ...systemQuery(asset ?? ({ id: assetId } as Asset)), enabled: !!asset && seesUpdates })
  const lowWarnings = sys?.usage && sys.usageAt && asset?.agentState === 'healthy' &&
    now() - Date.parse(sys.usageAt) <= LOW_FRESH_MS ? (sys.warnings ?? []) : []
  const diskStuck = disks ? stuck(disks) : false
  const [editing, setEditing] = useState(false)

  if (!asset) {
    return (
      <Box>
        <PageHeader crumbs={[{ label: 'Assets', to: '/assets' }]} title="Asset not found" />
        <PageBody>
          <Card>
            <EmptyState
              icon={IconServer2}
              title="No such asset."
              description="It may have been removed from the inventory, or the link is stale."
            />
          </Card>
        </PageBody>
      </Box>
    )
  }

  // A tab this type does not have (a link to Logs on a workstation, say)
  // lands on Overview rather than on a tab missing from the row above.
  const tab: ServerTab = TABS_BY_TYPE[assetTypeOf(asset)].includes(asked) ? asked : 'overview'
  const sessions = allSessions?.filter((s) => s.assetId === asset.id)
  const setTab = (t: ServerTab) =>
    void navigate({ search: t === 'overview' ? {} : { tab: t }, replace: true, resetScroll: false })
  const needsAttention = asset.hostKeyState !== 'pinned'

  return (
    <SplitView storageKey="servers" label="servers" list={<ServerList currentId={asset.id} />}>
      <PageHeader
        crumbs={[
          { label: 'Assets', to: '/assets' },
          { label: asset.hostname.split('.')[0] ?? asset.hostname },
        ]}
        title={asset.hostname}
        status={
          <Group gap={SP.snug} wrap="nowrap">
            <ReachBadge reach={reachOf(asset)} />
            {lowWarnings.length > 0 && (
              <Badge size="sm" variant="light" radius="sm" tt="none" data-testid="header-low"
                color={lowWarnings.some((w) => w.level === 'critical') ? 'rose' : 'amber'}
                title={lowWarnings.map(warningText).join('\n')}>
                Running low
              </Badge>
            )}
          </Group>
        }
        description={
          <Group gap={SP.snug} wrap="wrap">
            <Text span size="sm" c="dimmed">
              {ASSET_TYPE_LABEL[assetTypeOf(asset)].one} · {asset.os || (asset.protocol === 'rdp' ? 'Windows' : 'Linux')} · {asset.address}
            </Text>
            {asset.tags.map((t) => (
              <Badge key={t} size="xs" variant="outline" color="slate">{t}</Badge>
            ))}
          </Group>
        }
        actions={
          <>
            {canEdit && (
              <Button
                variant="default"
                size="sm"
                leftSection={<IconPencil size={16} />}
                onClick={() => setEditing(true)}
              >
                Edit
              </Button>
            )}
            <ButtonLink
              size="sm"
              to="/connect"
              search={{ host: asset.hostname }}
              leftSection={<IconPlugConnected size={18} />}
              disabled={asset.principals.length === 0}
            >
              Connect
            </ButtonLink>
          </>
        }
        tabs={
          <Tabs value={tab} onChange={(v) => v && setTab(v as ServerTab)} keepMounted={false}>
            <Tabs.List className="argus-tabs-scroll" style={{ borderBottom: 0 }}>
              {TABS_BY_TYPE[assetTypeOf(asset)].filter((t) => {
                // Updates, disks and logs are for those who may act on them, or audit.
                return (t !== 'updates' && t !== 'disks' && t !== 'logs') || seesUpdates
              }).map((t) => (
                <Tabs.Tab
                  key={t}
                  value={t}
                  fz="sm"
                  rightSection={
                    t === 'sessions' && sessions && sessions.length > 0 ? (
                      <Badge size="xs" variant="light" color="slate" circle={false}>{sessions.length}</Badge>
                    ) : t === 'updates' && waiting > 0 ? (
                      <Badge size="xs" variant="light" color={securityWaiting > 0 ? 'rose' : 'slate'} circle={false}>{waiting}</Badge>
                    ) : t === 'disks' && growRoom >= 1024 ** 3 ? (
                      <Badge size="xs" variant="light" color="teal" circle={false}>+{bytes(growRoom)}</Badge>
                    ) : t === 'disks' && diskStuck ? (
                      <Badge size="xs" variant="light" color="amber" circle={false}>full</Badge>
                    ) : t === 'logs' && freeable >= 1024 ** 3 ? (
                      <Badge size="xs" variant="light" color={filling ? 'amber' : 'slate'} circle={false}>{bytes(freeable)}</Badge>
                    ) : t === 'settings' && needsAttention ? (
                      <Box
                        component="span"
                        aria-label="Needs attention"
                        w={7}
                        h={7}
                        style={{ borderRadius: '50%', background: 'var(--mantine-color-amber-5)', display: 'inline-block' }}
                      />
                    ) : undefined
                  }
                >
                  {tabLabel(t, asset.kind)}
                </Tabs.Tab>
              ))}
            </Tabs.List>
          </Tabs>
        }
      />

      <PageBody>
        <HostKeyChanged asset={asset} />
        {tab === 'overview' && <OverviewTab asset={asset} sessions={sessions} onTab={setTab} canFreeSpace={seesUpdates && isConfigured()} />}
        {tab === 'access' && <AccessTab asset={asset} canEdit={canEdit} />}
        {tab === 'sessions' && <SessionsTab asset={asset} sessions={sessions} />}
        {tab === 'apps' && <AppsTab asset={asset} canEdit={canEdit} />}
        {tab === 'updates' && seesUpdates && <UpdatesTab asset={asset} canEdit={canEdit} />}
        {tab === 'disks' && seesUpdates && <DisksTab asset={asset} canEdit={canEdit} />}
        {tab === 'logs' && seesUpdates && <LogsTab asset={asset} canEdit={canEdit} />}
        {tab === 'agent' && <AgentTab asset={asset} canEdit={canEdit} />}
        {tab === 'settings' && (
          <SettingsTab asset={asset} canEdit={canEdit} onEdit={() => setEditing(true)} />
        )}
      </PageBody>

      <Suspense fallback={null}>
        {editing && canEdit && (
          <AssetForm opened={editing} onClose={() => setEditing(false)} asset={asset} />
        )}
      </Suspense>
    </SplitView>
  )
}
