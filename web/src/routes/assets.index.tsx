import { useState } from 'react'
import { Box, Group, Select, Table, Text, TextInput, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconSearch, IconServer2 } from '@tabler/icons-react'
import { DataTable, EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import {
  AgentBadge, BypassBadge, CredentialBadge, HealthDot, HostKeyBadge, Mono,
  relTime, rowNav,
} from '~/components/primitives'
import { FS, SP } from '~/theme'
import { assetsQuery, groupsQuery } from '~/lib/queries'
import type { Asset } from '~/types/domain'
import { EPOCH } from '~/lib/seed'
import { isConfigured } from '~/lib/live'

export const Route = createFileRoute('/assets/')({
  component: Assets,
  loader: ({ context }) => context.queryClient.ensureQueryData(assetsQuery({})),
})

function rotationOverdue(a: Asset): boolean {
  if (!a.credentialRotatedAt || !a.rotationIntervalDays) return false
  const ref = isConfigured() ? Date.now() : EPOCH
  return ref - Date.parse(a.credentialRotatedAt) > a.rotationIntervalDays * 86_400_000
}

function Assets() {
  const navigate = useNavigate()
  const [search, setSearch] = useState('')
  const [groupId, setGroupId] = useState<string | null>(null)
  const [hostKeyState, setHostKeyState] = useState<string | null>(null)

  const { data: groups } = useQuery(groupsQuery())
  const { data: assets } = useQuery(
    assetsQuery({
      search,
      groupId,
      hostKeyState: hostKeyState as Asset['hostKeyState'] | null,
    }),
  )

  const filtered = Boolean(search.trim() || groupId || hostKeyState)

  return (
    <Box>
      <PageHeader
        title="Assets"
        description="Every host Argus can broker a session to. Host-key state is the trust anchor — an unpinned target is one nobody has verified."
      />

      <PageBody>
        <Toolbar
          right={
            <Text size={FS.micro} c="dimmed">
              {assets?.length ?? 0} assets
            </Text>
          }
        >
          <TextInput
            w={280}
            placeholder="Filter by hostname, address, OS or tag"
            leftSection={<IconSearch size={14} />}
            value={search}
            onChange={(e) => setSearch(e.currentTarget.value)}
          />
          <Select
            w={190}
            placeholder="All groups"
            clearable
            value={groupId}
            onChange={setGroupId}
            data={groups?.map((g) => ({ value: g.id, label: `${g.name} (${g.assetCount})` })) ?? []}
          />
          <Select
            w={170}
            placeholder="Any host key state"
            clearable
            value={hostKeyState}
            onChange={setHostKeyState}
            data={[
              { value: 'pinned', label: 'Pinned' },
              { value: 'unpinned', label: 'Unpinned' },
              { value: 'changed', label: 'Changed' },
            ]}
          />
        </Toolbar>

        <DataTable
          minWidth={980}
          loading={assets === undefined}
          isEmpty={assets?.length === 0}
          columns={['Host', 'Address', 'OS', 'Auth', 'Host key', 'Agent', 'Bypass', 'Rotated']}
          empty={
            <EmptyState
              icon={IconServer2}
              title="No assets match."
              description={
                filtered
                  ? 'Nothing in the inventory fits these filters. Clear one to widen the search.'
                  : 'The inventory is empty. Enrol a host from Coverage, or add one with argus-control assets add.'
              }
            />
          }
        >
          {assets?.map((a) => (
            <Table.Tr
              key={a.id}
              {...rowNav(() => navigate({ to: '/assets/$assetId', params: { assetId: a.id } }))}
            >
              <Table.Td>
                <Group gap={SP.cozy} wrap="nowrap">
                  <HealthDot health={a.health} />
                  <Box>
                    <Mono>{a.hostname.split('.')[0]}</Mono>
                    <Text size={FS.micro} c="dimmed">
                      {a.hostname.split('.').slice(1).join('.')}
                    </Text>
                  </Box>
                </Group>
              </Table.Td>
              <Table.Td><Mono c="dimmed">{a.address}:{a.port}</Mono></Table.Td>
              <Table.Td><Text size="xs" c="dimmed">{a.os}</Text></Table.Td>
              <Table.Td><CredentialBadge mode={a.credentialMode} /></Table.Td>
              <Table.Td><HostKeyBadge state={a.hostKeyState} /></Table.Td>
              <Table.Td>
                <AgentBadge state={a.agentState} lastSeen={a.agentLastSeenAt} />
              </Table.Td>
              <Table.Td>
                <BypassBadge posture={a.bypassPosture} unmanagedKeys={a.unmanagedKeyCount} />
              </Table.Td>
              <Table.Td>
                {a.credentialMode === 'ca-certificate' ? (
                  <Tooltip label="Certificate auth — nothing to rotate">
                    <Text size="xs" c="dimmed">n/a</Text>
                  </Tooltip>
                ) : (
                  <Text size="xs" c={rotationOverdue(a) ? 'amber.4' : 'dimmed'}>
                    {relTime(a.credentialRotatedAt)}
                    {rotationOverdue(a) && ' ⚠'}
                  </Text>
                )}
              </Table.Td>
            </Table.Tr>
          ))}
        </DataTable>
      </PageBody>
    </Box>
  )
}
