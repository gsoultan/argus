import { Alert, Badge, Box, Card, Group, SegmentedControl, SimpleGrid, Stack, Table, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, Link } from '@tanstack/react-router'
import { IconShieldCheck } from '@tabler/icons-react'
import { useState } from 'react'
import { DataTable, EmptyState, PageBody, PageHeader, Toolbar } from '~/components/page'
import { firewallName, firewallOff, openWords, type FirewallFleetRow } from '~/lib/firewallApi'
import { firewallFleetQuery } from '~/lib/firewallQueries'
import { guard } from '~/lib/guard'
import { isConfigured } from '~/lib/live'
import { FS } from '~/theme'

export const Route = createFileRoute('/assets/firewalls')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  component: FleetFirewalls,
})

type Show = 'all' | 'off' | 'open'

/**
 * Assets › Firewalls: every server's own firewall on one page. Which ones are
 * off, which let the internet reach something, and how many rules were made
 * outside Argus. Each server links to its Firewall tab, where the rules are.
 */
function FleetFirewalls() {
  const [show, setShow] = useState<Show>('all')
  const { data, isLoading, error } = useQuery(firewallFleetQuery())
  const rows = data?.servers ?? []
  const off = rows.filter(firewallOff)
  const open = rows.filter((r) => r.openToInternet.length > 0)
  const on = rows.filter((r) => r.reported && r.active)
  const shown = show === 'off' ? off : show === 'open' ? open : rows

  return (
    <Box>
      <PageHeader
        crumbs={[{ label: 'Assets', to: '/assets' }, { label: 'Firewalls' }]}
        title="Firewalls"
        description="The firewall on each server and workstation, as its agent last read it. Open one to see its rules and who can reach each port."
      />
      <PageBody>
        {!isConfigured() ? (
          <Alert color="slate"><Text size={FS.body}>Connect this console to Argus to see each server's firewall.</Text></Alert>
        ) : error ? (
          <Alert color="rose"><Text size={FS.body}>Could not read the firewalls: {error.message}</Text></Alert>
        ) : (
          <Stack gap="lg">
            <SimpleGrid cols={{ base: 1, xs: 3 }} spacing="md">
              <Tile label="Firewall on" value={on.length} of={rows.length} />
              <Tile label="Off or none" value={off.length} color={off.length > 0 ? 'amber' : undefined} />
              <Tile label="Open to the internet" value={open.length} color={open.length > 0 ? 'rose' : undefined} />
            </SimpleGrid>
            <Toolbar>
              <SegmentedControl
                value={show}
                onChange={(v) => setShow(v as Show)}
                aria-label="Show"
                data={[
                  { label: `All ${rows.length}`, value: 'all' },
                  { label: `Off or none ${off.length}`, value: 'off' },
                  { label: `Open to the internet ${open.length}`, value: 'open' },
                ]}
              />
            </Toolbar>
            <DataTable
              columns={['Server', 'Firewall', 'Rules', 'Open to the internet']}
              minWidth={640}
              loading={isLoading}
              isEmpty={shown.length === 0}
              empty={<EmptyState icon={IconShieldCheck} title={show === 'all' ? 'No servers yet.' : 'None.'}
                description={show === 'all' ? 'Servers and workstations appear here once their agent reports.' : 'Nothing here needs a look.'} />}
              footer="Read only for now. Changing rules from Argus comes next."
            >
              {shown.map((r) => <Row key={r.assetId} r={r} />)}
            </DataTable>
          </Stack>
        )}
      </PageBody>
    </Box>
  )
}

function Tile({ label, value, of, color }: { label: string; value: number; of?: number; color?: string }) {
  return (
    <Card>
      <Text size={FS.meta} c="dimmed">{label}</Text>
      <Group gap={6} align="baseline">
        <Text fz={28} fw={700} c={color}>{value}</Text>
        {of !== undefined && <Text size={FS.meta} c="dimmed">of {of}</Text>}
      </Group>
    </Card>
  )
}

function Row({ r }: { r: FirewallFleetRow }) {
  return (
    <Table.Tr>
      <Table.Td>
        <Link to="/assets/$assetId" params={{ assetId: r.assetId }} search={{ tab: 'firewall' }}>
          <Text size="sm" fw={600} ff="monospace">{r.hostname}</Text>
        </Link>
        <Text size={FS.micro} c="dimmed">{r.os || r.kind}</Text>
      </Table.Td>
      <Table.Td>
        {!r.reported ? (
          <Text size="sm" c="dimmed">Not read yet</Text>
        ) : r.error ? (
          <Text size="sm" c="rose">Could not read</Text>
        ) : !r.firewall ? (
          <Badge color="amber" variant="light">No firewall</Badge>
        ) : (
          <Group gap={6}>
            <Text size="sm">{firewallName(r.firewall)}</Text>
            <Badge color={r.active ? 'teal' : 'amber'} variant="light">{r.active ? 'On' : 'Off'}</Badge>
            {r.docker && <Badge color="slate" variant="light">Docker</Badge>}
          </Group>
        )}
      </Table.Td>
      <Table.Td>
        {r.reported && !r.error && (
          <Text size="sm">
            {r.rules}
            {r.outside > 0 && <Text span c="dimmed">{`, ${r.outside} by hand`}</Text>}
          </Text>
        )}
      </Table.Td>
      <Table.Td>
        {r.openToInternet.length > 0 ? (
          <Text size="sm" c="rose">{openWords(r.openToInternet)}</Text>
        ) : r.reported && !r.error ? (
          <Text size="sm" c="dimmed">Nothing</Text>
        ) : null}
      </Table.Td>
    </Table.Tr>
  )
}
