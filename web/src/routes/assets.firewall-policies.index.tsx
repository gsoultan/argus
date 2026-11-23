import { Alert, Badge, Box, Button, Card, Group, Modal, Select, Stack, Table, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { AnchorLink } from '~/components/links'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconPlus, IconShieldLock } from '@tabler/icons-react'
import { useState } from 'react'
import { DataTable, EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { changeStateColor, lineWords, type FirewallPolicy } from '~/lib/firewallApi'
import { firewallPoliciesQuery, firewallStartersQuery, useSaveFirewallPolicy } from '~/lib/firewallQueries'
import { guard } from '~/lib/guard'
import { isConfigured } from '~/lib/live'
import { run as notifyRun } from '~/lib/notify'
import { meQuery } from '~/lib/queries'
import { FS } from '~/theme'

export const Route = createFileRoute('/assets/firewall-policies/')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  component: FirewallPolicies,
})

/**
 * Assets › Firewalls › Policies: named sets of firewall lines, like AWS
 * security groups, put on servers by name or on every server with a tag.
 * A change goes out to one server first, then five at a time, and stops at
 * the first that fails.
 */
function FirewallPolicies() {
  const { data, isLoading, error } = useQuery(firewallPoliciesQuery())
  const { data: me } = useQuery(meQuery())
  const [making, setMaking] = useState(false)
  const admin = me?.role === 'admin' || me?.role === 'owner'
  const rows = data?.policies ?? []
  return (
    <Box>
      <PageHeader
        crumbs={[{ label: 'Assets', to: '/assets' }, { label: 'Firewalls', to: '/assets/firewalls' }, { label: 'Policies' }]}
        title="Firewall policies"
        description="Shared firewall lines for many servers. Put a policy on servers or on a tag; each server's firewall follows it."
      />
      <PageBody>
        {!isConfigured() ? (
          <Alert color="slate"><Text size={FS.body}>Connect this console to Argus to see firewall policies.</Text></Alert>
        ) : error ? (
          <Alert color="rose"><Text size={FS.body}>Could not read the policies: {error.message}</Text></Alert>
        ) : (
          <Stack gap="lg">
            <Toolbar right={admin ? (
              <Button leftSection={<IconPlus size={14} />} onClick={() => setMaking(true)} data-testid="policy-new">New policy</Button>
            ) : undefined}>
              <Text size={FS.meta} c="dimmed">{rows.length} {rows.length === 1 ? 'policy' : 'policies'}</Text>
            </Toolbar>
            <DataTable
              columns={['Policy', 'Lines', 'Where', 'Servers']}
              minWidth={720}
              loading={isLoading}
              isEmpty={rows.length === 0}
              empty={<EmptyState icon={IconShieldLock} title="No policies yet."
                description="Start from Linux base, Windows base or Locked down, or write your own." />}
            >
              {rows.map((p) => <Row key={p.id} p={p} />)}
            </DataTable>
          </Stack>
        )}
      </PageBody>
      {making && <NewPolicy onClose={() => setMaking(false)} />}
    </Box>
  )
}

function Row({ p }: { p: FirewallPolicy }) {
  const where = p.targets.map((t) => (t.tag ? `tag ${t.tag}` : t.hostname)).join(', ')
  return (
    <Table.Tr>
      <Table.Td>
        <AnchorLink to="/assets/firewall-policies/$policyId" params={{ policyId: p.id }} fw={600} size="sm">{p.name}</AnchorLink>
        <Text size={FS.micro} c="dimmed">Version {p.version}{p.description ? ` · ${p.description}` : ''}</Text>
      </Table.Td>
      <Table.Td>
        <Text size={FS.meta} lineClamp={2}>
          {p.lines.map(lineWords).join('; ') || 'No lines'}
          {p.defaultOut === 'deny' ? '; nothing else goes out' : ''}
        </Text>
      </Table.Td>
      <Table.Td><Text size={FS.meta} c={where ? undefined : 'dimmed'}>{where || 'Nowhere yet'}</Text></Table.Td>
      <Table.Td>
        <Group gap={6} wrap="nowrap">
          <Text size="sm">{p.current} of {p.servers}</Text>
          {p.rollout && p.rollout.state !== 'done' && (
            <Badge size="sm" variant="light" radius="sm" tt="none"
              color={p.rollout.state === 'running' ? 'azure' : p.rollout.state === 'awaiting_approval' ? 'amber' : changeStateColor('stopped')}>
              {p.rollout.state === 'running' ? 'Rolling out' : p.rollout.state === 'awaiting_approval' ? 'Waiting for approval' : `Rollout ${p.rollout.state}`}
            </Badge>
          )}
        </Group>
      </Table.Td>
    </Table.Tr>
  )
}

function NewPolicy({ onClose }: { onClose: () => void }) {
  const { data } = useQuery(firewallStartersQuery())
  const [from, setFrom] = useState<string>('')
  const [name, setName] = useState('')
  const save = useSaveFirewallPolicy()
  const navigate = useNavigate()
  const starters = data?.starters ?? []
  const start = starters.find((s) => s.name === from)
  return (
    <Modal opened onClose={onClose} title={<Text fw={700}>New firewall policy</Text>}>
      <Stack gap="md">
        <Select label="Start from" value={from} allowDeselect={false} onChange={(v) => { setFrom(v ?? ''); if (v) setName(v) }}
          data={[{ value: '', label: 'Nothing: write the lines myself' }, ...starters.map((s) => ({ value: s.name, label: s.name }))]} />
        {start && (
          <Card padding="sm" withBorder>
            <Text size={FS.body}>{start.description}</Text>
          </Card>
        )}
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80} data-autofocus />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!name.trim()} loading={save.isPending}
            onClick={() => {
              let made: FirewallPolicy | undefined
              void notifyRun(async () => {
                made = await save.mutateAsync({
                  name: name.trim(), description: start?.description ?? '', lines: start?.lines ?? [],
                  defaultOut: start?.defaultOut ?? 'allow', starter: !!start,
                })
              }, { failure: 'Not made', success: ['Made', 'Put it on servers or a tag to use it.'] }).then((ok) => {
                if (ok && made) void navigate({ to: '/assets/firewall-policies/$policyId', params: { policyId: made.id } })
              })
            }}>
            Make it
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
