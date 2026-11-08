import { Alert, Badge, Box, Button, Card, Skeleton, Stack, Table, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { IconCircleCheck, IconRefresh, IconShieldExclamation } from '@tabler/icons-react'
import { EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { guard } from '~/lib/guard'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import { useStartPackageRuns } from '~/lib/packageQueries'
import { meQuery, useUpdateAgentFleet } from '~/lib/queries'
import { updatesWaitingQuery, type UpdatesWaitingRow } from '~/lib/updatesWaiting'
import { FS, SP } from '~/theme'

export const Route = createFileRoute('/assets/updates')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  component: FleetUpdates,
})

const AGENT_STATE: Record<NonNullable<UpdatesWaitingRow['agent']>, string> = {
  behind: 'behind',
  updating: 'updating',
  held: 'held back',
  cannot: 'cannot update itself',
  too_old: 'too old to update from here',
}

/**
 * Assets › Updates: everything waiting to be updated across the fleet, in
 * one place. Security fixes first, then agents, then other updates and
 * restarts. Each asset links to its own Packages or Agent tab, where the
 * single-asset controls are; the buttons here act on the whole group.
 */
function FleetUpdates() {
  const { data: me } = useQuery(meQuery())
  const canEdit = me?.role === 'admin' || me?.role === 'owner'
  const { data: v, isLoading, error } = useQuery({ ...updatesWaitingQuery(), enabled: isConfigured() })
  const updateAgents = useUpdateAgentFleet()
  const startRuns = useStartPackageRuns()

  const security = v?.assets.filter((r) => r.security > 0) ?? []
  const agents = v?.assets.filter((r) => r.agent) ?? []
  const other = v?.assets.filter((r) => r.security === 0 && r.updates > 0) ?? []
  const restart = v?.assets.filter((r) => r.rebootRequired) ?? []

  const list = (rows: UpdatesWaitingRow[], tab: 'updates' | 'agent', what: (r: UpdatesWaitingRow) => string) => (
    <Table.ScrollContainer minWidth={420}>
      <Table verticalSpacing="xs">
        <Table.Tbody>
          {rows.map((r) => (
            <Table.Tr key={r.assetId}>
              <Table.Td>
                <Link to="/assets/$assetId" params={{ assetId: r.assetId }} search={{ tab }}>
                  <Text size="sm" fw={600} ff="monospace">{r.hostname}</Text>
                </Link>
              </Table.Td>
              <Table.Td><Text size="sm" c="dimmed">{what(r)}</Text></Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </Table.ScrollContainer>
  )

  return (
    <Box>
      <PageHeader
        crumbs={[{ label: 'Assets', to: '/assets' }, { label: 'Updates' }]}
        title="Updates"
        description="Everything waiting to be updated across your assets. Agents update once nobody is connected."
      />
      <PageBody>
        {!isConfigured() ? (
          <Alert color="slate"><Text size={FS.body}>Connect this console to Argus to see what is waiting.</Text></Alert>
        ) : isLoading ? (
          <Skeleton h={320} />
        ) : error || !v ? (
          <Alert color="rose"><Text size={FS.body}>Could not read what is waiting: {error?.message}</Text></Alert>
        ) : v.assets.length === 0 ? (
          <Card>
            <EmptyState icon={IconCircleCheck} title="Everything is up to date."
              description="No security fixes, updates, agent releases or restarts are waiting." />
          </Card>
        ) : (
          <Stack gap="lg">
            {security.length > 0 && (
              <SectionCard title="Security fixes" icon={IconShieldExclamation} iconColor="rose"
                badge={<Badge color="rose" variant="light">{v.securityFixes}</Badge>}
                action={canEdit ? (
                  <Button size="xs" color="rose" loading={startRuns.isPending}
                    onClick={() => void run(() => startRuns.mutateAsync({ assetIds: security.map((r) => r.assetId), mode: 'security', reboot: 'never' }), {
                      failure: 'Not started',
                      success: ['Started', `Installing security fixes on ${security.length} ${security.length === 1 ? 'asset' : 'assets'}, as each one's update rules allow.`],
                    })}>
                    Install security fixes
                  </Button>
                ) : undefined}>
                {list(security, 'updates', (r) => `${r.security} security ${r.security === 1 ? 'fix' : 'fixes'}, ${r.updates} ${r.updates === 1 ? 'update' : 'updates'} in all`)}
              </SectionCard>
            )}
            {agents.length > 0 && (
              <SectionCard title={v.latest ? `Agent ${v.latest}` : 'Agents'} icon={IconRefresh}
                badge={<Badge color="azure" variant="light">{agents.length}</Badge>}
                action={canEdit && v.agentsBehind > 0 ? (
                  <Button size="xs" loading={updateAgents.isPending}
                    onClick={() => void run(() => updateAgents.mutateAsync({}), {
                      failure: 'Not asked',
                      success: ['Asked', 'Each agent updates once nobody is connected to its asset.'],
                    })}>
                    Update all agents
                  </Button>
                ) : undefined}>
                {list(agents, 'agent', (r) => `${r.agentVersion || 'unknown version'}, ${AGENT_STATE[r.agent!]}`)}
              </SectionCard>
            )}
            {other.length > 0 && (
              <SectionCard title="Other updates" badge={<Badge color="slate" variant="light">{other.length}</Badge>}>
                {list(other, 'updates', (r) => `${r.updates} ${r.updates === 1 ? 'update' : 'updates'}`)}
              </SectionCard>
            )}
            {restart.length > 0 && (
              <SectionCard title="Restart needed" badge={<Badge color="amber" variant="light">{restart.length}</Badge>}>
                {list(restart, 'updates', () => 'An update takes effect after a restart')}
              </SectionCard>
            )}
            <Text size={FS.meta} c="dimmed" mt={SP.snug}>
              Installing follows each asset's update rules in Settings, Updates, including approvals and windows.
            </Text>
          </Stack>
        )}
      </PageBody>
    </Box>
  )
}
