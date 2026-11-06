import { ApprovalBadge } from '~/components/ChangeImpact'
import { writesNeedApproval } from '~/lib/changeApproval'
import { Alert, Badge, Box, Button, Group, SegmentedControl, SimpleGrid, Stack, Table, Text, TextInput, UnstyledButton } from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconDatabase, IconInfoCircle, IconPlus, IconSearch } from '@tabler/icons-react'
import { Fragment, useMemo, useState } from 'react'
import { DataTable, EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { LastBackupCell } from '~/components/DatabaseBackups'
import { LevelBadge } from '~/components/LevelBadge'
import { relTime } from '~/components/primitives'
import { databasesQuery, meQuery } from '~/lib/queries'
import { isConfigured } from '~/lib/live'
import { canSee } from '~/lib/roles'
import { engineOf, isBroker } from '~/lib/engines'
import { FS, INK } from '~/theme'
import { EngineMark } from '~/components/EngineMark'
import { loadRecentDatabases } from '~/lib/recentDatabases'
import type { DBAccess, DBEngine, DBLabel, Database, DatabaseBackup } from '~/types/domain'

/**
 * Databases: every database this person may open, one row each, with what
 * they can do on it. Open takes them straight into the workspace; the level
 * pill is the one piece of security they see every day.
 *
 * The control plane has already narrowed the list: an operator sees only the
 * databases some assignment of theirs reaches, an admin sees them all.
 */
export const Route = createFileRoute('/databases/')({
  component: Databases,
  loader: ({ context }) =>
    Promise.all([
      context.queryClient.ensureQueryData(meQuery()).catch(() => undefined),
      isConfigured() ? context.queryClient.ensureQueryData(databasesQuery()).catch(() => undefined) : undefined,
    ]),
})

export const LABEL_COLOR: Record<DBLabel, string> = { production: 'rose', staging: 'amber', development: 'teal' }

function describeAccess(acc: DBAccess | undefined): string {
  if (!acc) return ''
  if (acc.level && acc.grantExpires) return `until ${new Date(acc.grantExpires).toLocaleTimeString()}`
  if (acc.approvable) return `${acc.approvable} needs approval`
  return ''
}

const LABEL_ORDER: DBLabel[] = ['production', 'staging', 'development']
const LABEL_TITLE: Record<DBLabel, string> = { production: 'Production', staging: 'Staging', development: 'Development' }
// RabbitMQ is a message broker and is listed under Brokers.
const ENGINE_ORDER: DBEngine[] = ['postgres', 'mysql', 'mongodb', 'redis']

function displayName(d: Database): string {
  return d.engine === 'redis' ? `db ${d.dbname}` : d.dbname
}

function Databases() {
  const { data: me } = useQuery(meQuery())
  const { data, isPending, error } = useQuery(databasesQuery())
  const admin = canSee(me?.role, 'admin')
  const navigate = useNavigate()
  const [label, setLabel] = useState<'all' | DBLabel>('all')
  const [engine, setEngine] = useState<'all' | DBEngine>('all')
  const [q, setQ] = useState('')
  const [recentIds] = useState(loadRecentDatabases)
  const narrow = useMediaQuery('(max-width: 48em)')

  // Message brokers have their own page, so the databases are the rest.
  const all = useMemo(() => (data?.databases ?? []).filter((d) => !isBroker(d.engine)), [data])

  const rows = useMemo(() => {
    const list = all
    const needle = q.trim().toLowerCase()
    return list.filter((d) =>
      (label === 'all' || d.label === label) &&
      (engine === 'all' || d.engine === engine) &&
      (!needle || d.dbname.toLowerCase().includes(needle) || d.hostname.toLowerCase().includes(needle)),
    )
  }, [all, label, engine, q])

  // Grouped by environment, production first, so which world a row is in
  // is the first thing read, not one badge among six columns.
  const groups = useMemo(() => LABEL_ORDER
    .map((l) => ({ label: l, items: rows.filter((d) => d.label === l) }))
    .filter((g) => g.items.length > 0), [rows])

  const engines = useMemo(() => {
    const list = all
    return ENGINE_ORDER.map((e) => ({ engine: e, n: list.filter((d) => d.engine === e).length })).filter((x) => x.n > 0)
  }, [all])

  // Opened recently in this browser, and still listed for this person.
  const recent = useMemo(() => {
    const byId = new Map(all.map((d) => [d.id, d]))
    return recentIds.flatMap((r) => {
      const d = byId.get(r.id)
      return d ? [{ d, at: r.at }] : []
    }).slice(0, 3)
  }, [all, recentIds])

  if (!isConfigured()) {
    return (
      <Box>
        <PageHeader title="Databases" description="Open a Postgres database or a Redis in the browser. Everything is recorded." />
        <PageBody>
          <Alert icon={<IconInfoCircle size={16} />} color="amber" variant="light">
            No control plane is configured, so there are no databases to show. This page has no sample data on
            purpose: it would look exactly like a real fleet.
          </Alert>
        </PageBody>
      </Box>
    )
  }

  const open = (d: Database) => void navigate({ to: '/databases/$dbId', params: { dbId: d.id } })

  return (
    <Box>
      <PageHeader
        title="Databases"
        description="Open a Postgres database or a Redis you've been given. Argus signs you in for this session only and records every statement and command. Message brokers are under Brokers."
        actions={
          admin ? (
            <ButtonLink to="/assets" size="sm" variant="default" leftSection={<IconPlus size={16} />}>
              Add from a server's page
            </ButtonLink>
          ) : undefined
        }
      />
      <PageBody>
        <Stack gap="md">
          {recent.length > 0 && (
            <Box>
              <Text size={FS.micro} c="dimmed" fw={600} tt="uppercase" mb={6} style={{ letterSpacing: '.05em' }}>
                Opened recently in this browser
              </Text>
              <SimpleGrid cols={{ base: 1, sm: 2, lg: 3 }} spacing="sm">
                {recent.map(({ d, at }) => (
                  <UnstyledButton key={d.id} onClick={() => open(d)} aria-label={`Open ${displayName(d)} on ${d.hostname}`}
                    className="argus-recent-db"
                    style={{ display: 'flex', alignItems: 'center', gap: 10, padding: '10px 12px', borderRadius: 10,
                      border: '1px solid var(--color-line)', background: 'var(--color-surface)', minWidth: 0 }}>
                    <EngineMark engine={d.engine} size={30} />
                    <Box style={{ minWidth: 0, flex: 1 }}>
                      <Group gap={6} wrap="nowrap">
                        <Text fw={600} size={FS.body} truncate>{displayName(d)}</Text>
                        <Badge size="xs" variant="light" color={LABEL_COLOR[d.label]} radius="sm">{d.label}</Badge>
                      </Group>
                      <Text size={FS.micro} c="dimmed" truncate>{d.hostname} · opened {relTime(new Date(at).toISOString())}</Text>
                    </Box>
                    <Text size={FS.meta} fw={600} c={INK.brand} style={{ whiteSpace: 'nowrap' }}>Open →</Text>
                  </UnstyledButton>
                ))}
              </SimpleGrid>
            </Box>
          )}
          <Toolbar
            right={
              <Text size={FS.meta} c="dimmed">
                {data ? `${rows.length} of ${all.length}` : null}
              </Text>
            }
          >
            <TextInput
              placeholder="Find a database or server"
              leftSection={<IconSearch size={14} />}
              value={q}
              onChange={(e) => setQ(e.currentTarget.value)}
              w={{ base: '100%', sm: 320 }}
              aria-label="Find a database"
            />
            <SegmentedControl
              value={label}
              onChange={(v) => setLabel(v as 'all' | DBLabel)}
              aria-label="Environment"
              data={[
                { label: 'All', value: 'all' },
                { label: 'Production', value: 'production' },
                { label: 'Staging', value: 'staging' },
                { label: 'Development', value: 'development' },
              ]}
            />
            {engines.length > 1 && (
              <SegmentedControl
                value={engine}
                onChange={(v) => setEngine(v as 'all' | DBEngine)}
                aria-label="Engine"
                data={[
                  { label: 'Every engine', value: 'all' },
                  ...engines.map((e) => ({ label: `${engineOf(e.engine).label} ${e.n}`, value: e.engine })),
                ]}
              />
            )}
          </Toolbar>
          {error && <Alert color="rose" variant="light">{(error as Error).message}</Alert>}
          {narrow && !isPending ? (
            <Stack gap="xs">
              {rows.length === 0 && (
                <Text size={FS.meta} c="dimmed">{data && all.length > 0 ? 'No database matches.' : 'No databases for you yet.'}</Text>
              )}
              {groups.map((g) => (
                <Box key={g.label}>
                  <Group gap={8} mt="xs" mb={6} wrap="nowrap">
                    <Box w={3} h={13} style={{ borderRadius: 2, background: `var(--mantine-color-${LABEL_COLOR[g.label]}-6)` }} />
                    <Text size={FS.meta} fw={650} tt="uppercase" style={{ letterSpacing: '.03em' }}>{LABEL_TITLE[g.label]}</Text>
                    <Text size={FS.micro} c="dimmed">{g.items.length}</Text>
                  </Group>
                  <Stack gap="xs">
                    {g.items.map((d) => <DatabaseCard key={d.id} d={d} acc={data?.access[d.id]} backup={data?.lastBackups?.[d.id]} onOpen={() => open(d)} />)}
                  </Stack>
                </Box>
              ))}
            </Stack>
          ) : <DataTable
            minWidth={820}
            striped={false}
            loading={isPending}
            isEmpty={rows.length === 0}
            empty={
              <EmptyState
                icon={IconDatabase}
                title={data && all.length > 0 ? 'No database matches' : admin ? 'No databases yet' : 'No databases for you yet'}
                description={
                  data && all.length > 0
                    ? 'Clear the search or pick another environment or engine.'
                    : admin
                      ? 'Add one from a server’s page. The agent on that server opens it; nothing else needs installing.'
                      : 'When an admin gives you a database it appears here. Nothing more to set up on your side.'
                }
              />
            }
            columns={['Database', 'Your access', 'Last backup', 'Agent', { label: '', hidden: true }]}
          >
            {groups.map((g) => (
              <Fragment key={g.label}>
                <Table.Tr aria-label={`${LABEL_TITLE[g.label]}, ${g.items.length}`}>
                  <Table.Td colSpan={5} py={6} style={{ background: 'var(--color-raised)' }}>
                    <Group gap={8} wrap="nowrap">
                      <Box w={3} h={14} style={{ borderRadius: 2, background: `var(--mantine-color-${LABEL_COLOR[g.label]}-6)` }} />
                      <Text size={FS.meta} fw={650}>{LABEL_TITLE[g.label]}</Text>
                      <Text size={FS.micro} c="dimmed">{g.items.length}</Text>
                    </Group>
                  </Table.Td>
                </Table.Tr>
                {g.items.map((d) => {
                  const acc = data?.access[d.id]
                  const canOpen = Boolean(acc?.level) && d.tunnelConnected
                  return (
                    <Table.Tr key={d.id} style={d.tunnelConnected ? undefined : { opacity: 0.72 }}>
                      <Table.Td>
                        <Group gap="sm" wrap="nowrap">
                          <EngineMark engine={d.engine} />
                          <Box style={{ minWidth: 0 }}>
                            <Group gap={6} wrap="nowrap">
                              <Text fw={600} size={FS.body}>{displayName(d)}</Text>
                              {writesNeedApproval(d) && <ApprovalBadge />}
                            </Group>
                            <Text size={FS.micro} c="dimmed" ff="monospace">{d.hostname} · {engineOf(d.engine).label}</Text>
                          </Box>
                        </Group>
                      </Table.Td>
                      <Table.Td><Group gap={6} wrap="nowrap">
                        <LevelBadge level={acc?.level} />
                        <Text size={FS.micro} c="dimmed">{describeAccess(acc)}</Text>
                      </Group></Table.Td>
                      <Table.Td>
                        {d.engine === 'postgres'
                          ? <LastBackupCell b={data?.lastBackups?.[d.id]} />
                          : <Text size={FS.meta} c="dimmed" title="Backups are for Postgres databases">—</Text>}
                      </Table.Td>
                      <Table.Td>
                        <Group gap={6} wrap="nowrap">
                          <Box w={7} h={7} style={{ borderRadius: 4, flexShrink: 0, background: d.tunnelConnected ? 'var(--mantine-color-teal-6)' : 'var(--mantine-color-rose-6)' }} />
                          <Text size={FS.meta} c={d.tunnelConnected ? 'dimmed' : INK.denied}>
                            {d.tunnelConnected ? 'connected' : 'offline'}
                          </Text>
                        </Group>
                      </Table.Td>
                      <Table.Td><Group justify="flex-end" gap="xs" wrap="nowrap">
                        {admin && (
                          <ButtonLink to="/databases/$dbId/settings" params={{ dbId: d.id }} size="xs" variant="subtle">
                            Who can connect
                          </ButtonLink>
                        )}
                        <Button
                          size="xs"
                          variant={acc?.level ? 'filled' : 'default'}
                          disabled={!canOpen && Boolean(acc?.level)}
                          title={!acc?.level ? 'Ask for access from the database page' : !d.tunnelConnected ? 'The agent is offline' : undefined}
                          onClick={() => open(d)}
                        >
                          {acc?.level ? 'Open' : 'Ask'}
                        </Button>
                      </Group></Table.Td>
                    </Table.Tr>
                  )
                })}
              </Fragment>
            ))}
          </DataTable>}
          <Text size={FS.micro} c="dimmed">
            Only databases you may open are listed. Your access is what you can do without asking; anything more is
            one request away. Last opened times are on each session under Activity{data ? `, as of ${relTime(new Date().toISOString())}` : ''}.
          </Text>
        </Stack>
      </PageBody>
    </Box>
  )
}

/** One database on a phone: a card instead of a table row too wide for the screen. */
function DatabaseCard({ d, acc, backup, onOpen }: { d: Database; acc?: DBAccess; backup?: DatabaseBackup; onOpen: () => void }) {
  const canOpen = Boolean(acc?.level) && d.tunnelConnected
  return (
    <Box p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 12, background: 'var(--color-surface)', opacity: d.tunnelConnected ? 1 : 0.72 }}>
      <Group gap="sm" wrap="nowrap">
        <EngineMark engine={d.engine} />
        <Box style={{ minWidth: 0, flex: 1 }}>
          <Text fw={600} size={FS.body} truncate>{displayName(d)}</Text>
          <Text size={FS.micro} c="dimmed" ff="monospace" truncate>{d.hostname} · {engineOf(d.engine).label}</Text>
        </Box>
        {acc?.level && !d.tunnelConnected
          ? <Text size={FS.meta} c={INK.denied}>offline</Text>
          : <Button size="xs" variant={acc?.level ? 'filled' : 'default'} disabled={!canOpen && Boolean(acc?.level)} onClick={onOpen}>
              {acc?.level ? 'Open' : 'Ask'}
            </Button>}
      </Group>
      <Group gap="sm" mt="xs" pt="xs" wrap="wrap" style={{ borderTop: '1px solid var(--color-line)' }}>
        <LevelBadge level={acc?.level} />
        {writesNeedApproval(d) && <ApprovalBadge size="xs" />}
        {d.engine === 'postgres' && (
          <Group gap={4} wrap="nowrap"><Text size={FS.micro} c="dimmed">Backup</Text><LastBackupCell b={backup} /></Group>
        )}
      </Group>
    </Box>
  )
}
