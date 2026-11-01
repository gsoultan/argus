import { Alert, Badge, Box, Button, Group, SegmentedControl, Stack, Table, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconDatabase, IconInfoCircle, IconLock, IconPlus, IconSearch } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { DataTable, EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { ButtonLink } from '~/components/links'
import { relTime } from '~/components/primitives'
import { databasesQuery, meQuery } from '~/lib/queries'
import { isConfigured } from '~/lib/live'
import { canSee } from '~/lib/roles'
import { FS } from '~/theme'
import type { DBAccess, DBLabel, DBLevel, Database } from '~/types/domain'

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

export function LevelBadge({ level, approval }: { level: DBLevel | '' | undefined; approval?: boolean }) {
  if (!level) return <Text size={FS.meta} c="dimmed">none</Text>
  const label = { read: 'Read', write: 'Write', owner: 'Owner', dba: 'DBA' }[level]
  return (
    <Badge variant="light" color={level === 'read' ? 'slate' : 'azure'} radius="sm"
      leftSection={approval ? <IconLock size={10} /> : undefined}>
      {label}
    </Badge>
  )
}

function describeAccess(acc: DBAccess | undefined): string {
  if (!acc) return ''
  if (acc.level && acc.grantExpires) return `until ${new Date(acc.grantExpires).toLocaleTimeString()}`
  if (acc.approvable) return `${acc.approvable} needs approval`
  return ''
}

function Databases() {
  const { data: me } = useQuery(meQuery())
  const { data, isPending, error } = useQuery(databasesQuery())
  const admin = canSee(me?.role, 'admin')
  const navigate = useNavigate()
  const [label, setLabel] = useState<'all' | DBLabel>('all')
  const [q, setQ] = useState('')

  const rows = useMemo(() => {
    const list = data?.databases ?? []
    const needle = q.trim().toLowerCase()
    return list.filter((d) =>
      (label === 'all' || d.label === label) &&
      (!needle || d.dbname.toLowerCase().includes(needle) || d.hostname.toLowerCase().includes(needle)),
    )
  }, [data, label, q])

  if (!isConfigured()) {
    return (
      <Box>
        <PageHeader title="Databases" description="Open a database in the browser. Every statement is recorded." />
        <PageBody>
          <Alert icon={<IconInfoCircle size={16} />} color="amber" variant="light">
            No control plane is configured, so there are no databases to show. This page has no sample data on
            purpose: it would look exactly like a real fleet.
          </Alert>
        </PageBody>
      </Box>
    )
  }

  return (
    <Box>
      <PageHeader
        title="Databases"
        description="Open a database you've been given. Argus signs you in for this session only and records every statement."
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
          <Toolbar
            right={
              <Text size={FS.meta} c="dimmed">
                {data ? `${rows.length} of ${data.databases.length}` : null}
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
              data={[
                { label: 'All', value: 'all' },
                { label: 'Production', value: 'production' },
                { label: 'Staging', value: 'staging' },
                { label: 'Development', value: 'development' },
              ]}
            />
          </Toolbar>
          {error && <Alert color="rose" variant="light">{(error as Error).message}</Alert>}
          <DataTable
            minWidth={720}
            loading={isPending}
            isEmpty={rows.length === 0}
            empty={
              <EmptyState
                icon={IconDatabase}
                title={admin ? 'No databases yet' : 'No databases for you yet'}
                description={
                  admin
                    ? 'Add one from a server’s page. The agent on that server opens it; nothing else needs installing.'
                    : 'When an admin gives you a database it appears here. Nothing more to set up on your side.'
                }
              />
            }
            columns={['Database', 'Engine', 'Label', 'Your access', 'Agent', { label: '', hidden: true }]}
          >
            {rows.map((d: Database) => {
              const acc = data?.access[d.id]
              const canOpen = Boolean(acc?.level) && d.tunnelConnected
              return (
                <Table.Tr key={d.id}>
                <Table.Td>
                <Group gap="xs" wrap="nowrap">
                  <IconDatabase size={16} color="var(--color-brand)" />
                  <Box>
                    <Text fw={600} size={FS.body}>{d.dbname}</Text>
                    <Text size={FS.micro} c="dimmed" ff="monospace">{d.hostname}</Text>
                  </Box>
                </Group>
                </Table.Td>
                <Table.Td><Text size={FS.meta} c="dimmed">Postgres</Text></Table.Td>
                <Table.Td><Badge variant="light" color={LABEL_COLOR[d.label]} radius="sm">{d.label}</Badge></Table.Td>
                <Table.Td><Group gap={6} wrap="nowrap">
                  <LevelBadge level={acc?.level} />
                  <Text size={FS.micro} c="dimmed">{describeAccess(acc)}</Text>
                </Group></Table.Td>
                <Table.Td><Text size={FS.meta} c={d.tunnelConnected ? 'dimmed' : 'rose'}>
                  {d.tunnelConnected ? 'connected' : 'offline'}
                </Text></Table.Td>
                <Table.Td><Group justify="flex-end" gap="xs" wrap="nowrap">
                  {admin && (
                    <ButtonLink to="/databases/$dbId/settings" params={{ dbId: d.id }} size="xs" variant="subtle">
                      Who can connect
                    </ButtonLink>
                  )}
                  <Button
                    size="xs"
                    disabled={!canOpen && Boolean(acc?.level)}
                    title={!acc?.level ? 'Ask for access from the database page' : !d.tunnelConnected ? 'The agent is offline' : undefined}
                    onClick={() => void navigate({ to: '/databases/$dbId', params: { dbId: d.id } })}
                  >
                    {acc?.level ? 'Open' : 'Ask'}
                  </Button>
                </Group></Table.Td>
                </Table.Tr>
              )
            })}
          </DataTable>
          <Text size={FS.micro} c="dimmed">
            Only databases you may open are listed. Your access is what you can do without asking; anything more is
            one request away. Last opened times are on each session under Activity{data ? `, as of ${relTime(new Date().toISOString())}` : ''}.
          </Text>
        </Stack>
      </PageBody>
    </Box>
  )
}
