import { Alert, Badge, Box, Group, SimpleGrid, Stack, Table, Text, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconAlertTriangle, IconBrandCloudflare, IconInfoCircle, IconTopologyStar3 } from '@tabler/icons-react'
import { useMemo } from 'react'
import { AnchorLink } from '~/components/links'
import { DataTable, EmptyState, PageBody, PageHeader, SectionCard } from '~/components/page'
import { relTime } from '~/components/primitives'
import { Figure } from '~/components/Websites'
import { CloudflareCard, CloudVPNsCard } from '~/components/NetworksCloud'
import { ServerNetworksCard } from '~/components/NetworksWireGuard'
import { IPsecLinksCard } from '~/components/NetworksIPsec'
import { isConfigured } from '~/lib/live'
import { meQuery } from '~/lib/queries'
import { formatBytes, linksOf, networksQuery, PEER_STATE, type Link, type NetworkTunnel, type NetworksView } from '~/lib/networks'
import { FS, SP } from '~/theme'

/**
 * Networks: every VPN link and tunnel on the servers Argus manages, as their
 * agents see them. WireGuard links between servers (matched by key, so a link
 * between two Argus servers is one row), Cloudflare Tunnels, and other VPN
 * software found. Read-only: setting links up comes later.
 */
export function NetworksPage() {
  const { data, isLoading, error } = useQuery({ ...networksQuery(), enabled: isConfigured() })
  const { data: me } = useQuery({ ...meQuery(), enabled: isConfigured() })
  const canManage = me?.role === 'admin' || me?.role === 'owner'
  const links = useMemo(() => linksOf(data?.interfaces ?? []), [data])
  const down = data?.problems.filter((p) => p.severity === 'down').length ?? 0

  return (
    <Box>
      <PageHeader
        title="Networks"
        description="VPN links and tunnels on your servers, in your clouds and in Cloudflare: WireGuard between servers, cloud site-to-site VPNs, Cloudflare Tunnels, and other VPN software. Checked every minute."
        status={data ? (down > 0
          ? <Badge color="rose" variant="filled">{down} down</Badge>
          : links.length + data.tunnels.length + data.cloudVpns.length > 0 ? <Badge color="teal" variant="light">All up</Badge> : undefined) : undefined}
      />
      <PageBody>
        {!isConfigured() ? (
          <Alert color="slate"><Text size={FS.body}>Connect this console to Argus to see the VPNs on your servers.</Text></Alert>
        ) : error ? (
          <Alert color="rose"><Text size={FS.body}>Could not read the networks: {error.message}</Text></Alert>
        ) : (
          <Stack gap="lg">
            {data && <Figures data={data} links={links} />}
            {data && data.problems.length > 0 && <Problems data={data} />}
            <LinksCard links={links} loading={isLoading} />
            <ServerNetworksCard canManage={canManage} me={me?.email} />
            <IPsecLinksCard canManage={canManage} me={me?.email} />
            <CloudVPNsCard vpns={data?.cloudVpns ?? []} loading={isLoading} />
            <TunnelsCard tunnels={data?.tunnels ?? []} loading={isLoading} />
            <CloudflareCard accounts={data?.cloudflare ?? []} loading={isLoading} canManage={canManage} />
            <SoftwareCard data={data} loading={isLoading} />
            {data && <HowItReads data={data} />}
          </Stack>
        )}
      </PageBody>
    </Box>
  )
}

function Figures({ data, links }: { data: NetworksView; links: Link[] }) {
  const linksUp = links.filter((l) => l.state === 'up').length
  const linksDown = links.filter((l) => l.state === 'down' || l.state === 'never').length
  const tunnelsDown = data.tunnels.filter((t) => t.state === 'down').length
  const reporting = data.servers.filter((s) => s.reported).length
  const vpnsDown = data.cloudVpns.filter((v) => v.state === 'down').length
  const cfTunnels = data.cloudflare.flatMap((a) => a.tunnels)
  const cfDown = cfTunnels.filter((t) => t.status === 'down').length
  return (
    <SimpleGrid cols={{ base: 2, sm: 3, lg: 6 }} spacing="md">
      <Figure label="WireGuard links" value={links.length === 0 ? 'None' : linksDown > 0 ? `${linksDown} down of ${links.length}` : `${linksUp} up of ${links.length}`} warn={linksDown > 0} />
      <Figure label="Cloudflare Tunnels" value={data.tunnels.length === 0 ? 'None' : tunnelsDown > 0 ? `${tunnelsDown} down of ${data.tunnels.length}` : `${data.tunnels.length} connected`} warn={tunnelsDown > 0} />
      <Figure label="Cloud VPNs" value={data.cloudVpns.length === 0 ? 'None' : vpnsDown > 0 ? `${vpnsDown} down of ${data.cloudVpns.length}` : `${data.cloudVpns.length} found`} warn={vpnsDown > 0} />
      <Figure label="In Cloudflare" value={data.cloudflare.length === 0 ? 'Not connected' : cfDown > 0 ? `${cfDown} down of ${cfTunnels.length}` : `${cfTunnels.length} tunnels`} warn={cfDown > 0} />
      <Figure label="VPN software found" value={`${data.software.length} on ${new Set(data.software.map((s) => s.assetId)).size} servers`} />
      <Figure label="Servers reporting links" value={`${reporting} of ${data.servers.length}`} />
    </SimpleGrid>
  )
}

function Problems({ data }: { data: NetworksView }) {
  return (
    <SectionCard title="Needs attention" icon={IconAlertTriangle} iconColor="rose">
      <Stack gap={SP.cozy}>
        {data.problems.map((p) => (
          <Group key={p.key} gap="sm" wrap="nowrap" align="flex-start" data-testid="network-problem">
            <Badge color={p.severity === 'down' ? 'rose' : 'amber'} variant={p.severity === 'down' ? 'filled' : 'light'} miw={56}>
              {p.severity === 'down' ? 'Down' : 'Check'}
            </Badge>
            <Box>
              <Text size={FS.body} fw={600}>{p.title}</Text>
              <Text size={FS.meta} c="dimmed">{p.detail}{p.assetId && <> <AnchorLink to="/assets/$assetId" params={{ assetId: p.assetId }} size={FS.meta}>Open {p.hostname}</AnchorLink></>}</Text>
            </Box>
          </Group>
        ))}
      </Stack>
    </SectionCard>
  )
}

function Server({ assetId, hostname }: { assetId?: string; hostname?: string }) {
  if (!assetId || !hostname) return null
  return <AnchorLink to="/assets/$assetId" params={{ assetId }} size={FS.body}>{hostname}</AnchorLink>
}

function StateBadge({ state }: { state: keyof typeof PEER_STATE }) {
  const s = PEER_STATE[state]
  return (
    <Tooltip label={s.hint} multiline w={260}>
      <Badge color={s.color} variant={s.color === 'rose' ? 'filled' : 'light'}>{s.label}</Badge>
    </Tooltip>
  )
}

function LinksCard({ links, loading }: { links: Link[]; loading: boolean }) {
  return (
    <SectionCard title="WireGuard links" icon={IconTopologyStar3} flush
      description="Each peer of each WireGuard interface. A link between two Argus servers is one row, named at both ends.">
      <DataTable columns={['From', 'To', 'State', 'Last handshake', 'Carries', 'Traffic']} minWidth={860}
        loading={loading} isEmpty={links.length === 0}
        empty={<EmptyState icon={IconTopologyStar3} title="No WireGuard links found."
          description="When a server runs WireGuard, its agent reports each peer here: whether it is up, its last handshake and its traffic." />}>
        {links.map((l) => (
          <Table.Tr key={l.key} data-testid="wg-link">
            <Table.Td><Server assetId={l.a.assetId} hostname={l.a.hostname} /> <Text span size={FS.meta} c="dimmed">{l.a.iface}</Text></Table.Td>
            <Table.Td>
              {l.b.hostname ? <><Server assetId={l.b.assetId} hostname={l.b.hostname} /> <Text span size={FS.meta} c="dimmed">{l.b.iface}</Text></>
                : <Text size={FS.body} ff="monospace">{l.b.endpoint || `${l.b.publicKey.slice(0, 10)}…`}</Text>}
            </Table.Td>
            <Table.Td><StateBadge state={l.state} /></Table.Td>
            <Table.Td><Text size={FS.body} c="dimmed">{l.latestHandshake ? relTime(l.latestHandshake) : 'Never'}</Text></Table.Td>
            <Table.Td><Text size={FS.meta} ff="monospace">{l.allowedIps.slice(0, 3).join(', ') || '—'}{l.allowedIps.length > 3 ? ` +${l.allowedIps.length - 3}` : ''}</Text></Table.Td>
            <Table.Td><Text size={FS.body}>{formatBytes(l.rxBytes)} in · {formatBytes(l.txBytes)} out</Text></Table.Td>
          </Table.Tr>
        ))}
      </DataTable>
    </SectionCard>
  )
}

const TUNNEL_STATE: Record<NetworkTunnel['state'], { label: string; color: string }> = {
  up: { label: 'Connected', color: 'teal' },
  down: { label: 'Not connected', color: 'rose' },
  unknown: { label: 'Not known', color: 'slate' },
}

function TunnelsCard({ tunnels, loading }: { tunnels: NetworkTunnel[]; loading: boolean }) {
  return (
    <SectionCard title="Cloudflare Tunnels" icon={IconBrandCloudflare} iconColor="amber" flush
      description="Each cloudflared on a server, read from its own health check on the server. Which tunnel it serves shows once your Cloudflare account is connected below.">
      <DataTable columns={['Server', 'State', 'Connections to Cloudflare', 'Tunnel and connector', 'cloudflared']} minWidth={760}
        loading={loading} isEmpty={tunnels.length === 0}
        empty={<EmptyState icon={IconBrandCloudflare} title="No Cloudflare Tunnels found."
          description="When cloudflared runs on a server, its agent reports whether it is connected to Cloudflare here." />}>
        {tunnels.map((t) => (
          <Table.Tr key={t.assetId + t.metrics} data-testid="cf-tunnel">
            <Table.Td><Server assetId={t.assetId} hostname={t.hostname} /></Table.Td>
            <Table.Td><Badge color={TUNNEL_STATE[t.state].color} variant={t.state === 'down' ? 'filled' : 'light'}>{TUNNEL_STATE[t.state].label}</Badge></Table.Td>
            <Table.Td><Text size={FS.body}>{t.error ? <Text span c="rose" size={FS.body}>{t.error}</Text> : t.readyConnections}</Text></Table.Td>
            <Table.Td>{t.tunnelName ? <Text size={FS.body} fw={500}>{t.tunnelName}</Text> : null}<Text size={FS.meta} ff="monospace" c="dimmed">{t.connectorId || '—'}</Text></Table.Td>
            <Table.Td><Text size={FS.body}>{t.version || '—'}</Text></Table.Td>
          </Table.Tr>
        ))}
      </DataTable>
    </SectionCard>
  )
}

function SoftwareCard({ data, loading }: { data?: NetworksView; loading: boolean }) {
  const list = data?.software ?? []
  return (
    <SectionCard title="VPN software on servers" icon={IconInfoCircle} flush
      description="Found by each agent's scan of what runs on its server. OpenVPN, IPsec, Tailscale and ZeroTier are watched only: Argus says they are there and whether they run.">
      <DataTable columns={['Server', 'Software', 'Version', 'Running', 'How it runs']} minWidth={700}
        loading={loading} isEmpty={list.length === 0}
        empty={<EmptyState icon={IconInfoCircle} title="No VPN software found on any server." />}>
        {list.map((s) => (
          <Table.Tr key={s.assetId + s.kind + (s.container ?? '')}>
            <Table.Td><Server assetId={s.assetId} hostname={s.hostname} /></Table.Td>
            <Table.Td><Text size={FS.body} fw={500}>{s.name}</Text></Table.Td>
            <Table.Td><Text size={FS.body}>{s.version || '—'}</Text></Table.Td>
            <Table.Td>{s.running ? <Badge color="teal" variant="light">Running</Badge>
              : <Badge color="slate" variant="light">{s.kind === 'wireguard' ? 'In the kernel' : 'Not running'}</Badge>}</Table.Td>
            <Table.Td><Text size={FS.meta} c="dimmed">{s.container ? `Container ${s.container}` : s.unit || '—'}</Text></Table.Td>
          </Table.Tr>
        ))}
      </DataTable>
    </SectionCard>
  )
}

function HowItReads({ data }: { data: NetworksView }) {
  const older = data.servers.filter((s) => !s.reported).length
  const noTool = data.servers.filter((s) => s.reported && !s.wgTool && data.interfaces.some((i) => i.assetId === s.assetId)).length
  return (
    <SectionCard title="How Argus reads these" icon={IconInfoCircle} iconColor="teal">
      <Stack gap={SP.cozy}>
        {[
          ['Each server reads its own links.', 'Its agent asks WireGuard for each peer\'s last handshake and traffic, and asks each cloudflared on the server whether it is connected. A link going up or down reaches this page within about a minute.'],
          ['No keys leave the server.', 'Only public keys, addresses and counters are read. Private and preshared keys, and cloudflared\'s credentials, are never read.'],
          ['Idle is not down.', 'A WireGuard link handshakes only when there is traffic, unless it keeps itself alive. Argus calls a quiet link down only when its side keeps it alive and knows where to dial.'],
          ['Clouds and Cloudflare are read, not changed.', 'Cloud VPNs come from the cloud sync with its read role; pre-shared keys are never read. Cloudflare is read with a read-only token kept sealed.'],
          ['Down goes to the bell.', 'A link, cloud VPN or tunnel going down raises an alert for admins, closed when it is back up.'],
        ].map(([t, d]) => (
          <Text key={t} size={FS.body}><Text span fw={600}>{t}</Text> <Text span c="dimmed">{d}</Text></Text>
        ))}
        {older > 0 && <Text size={FS.meta} c="amber">{older} {older === 1 ? 'server runs an agent' : 'servers run agents'} too old to report links. Update the agent to see them here.</Text>}
        {noTool > 0 && <Text size={FS.meta} c="dimmed">{noTool} {noTool === 1 ? 'server has' : 'servers have'} WireGuard without the wg tool, so only the interface is known, not its peers. Install wireguard-tools to see them.</Text>}
      </Stack>
    </SectionCard>
  )
}
