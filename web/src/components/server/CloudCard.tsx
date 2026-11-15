import { Badge, Box, Group, Stack, Text } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconCloud } from '@tabler/icons-react'
import { AnchorLink } from '~/components/links'
import { SectionCard } from '~/components/page'
import { Mono, absTime, relTime } from '~/components/primitives'
import { ENV_COLOR, ENV_LABEL, assetCloudQuery, envOf, stateColor, stateLabel, type AssetCloud } from '~/lib/cloud'
import { meQuery } from '~/lib/queries'
import { seesWholeFleet } from '~/lib/roles'
import { FS, INK } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * A server's cloud side, on its Overview: the instance it runs as, and how
 * Argus knows. Fleet roles only, like Cloud, and only with a control plane:
 * cloud accounts are never drawn from sample data. Nothing is drawn for a
 * server that has no instance and whose agent names none.
 */
export function CloudCard({ asset }: { asset: Asset }) {
  const { data: me } = useQuery(meQuery())
  const role = me?.role
  const { data } = useQuery(assetCloudQuery(asset.id, seesWholeFleet(role)))
  if (!data || (!data.instance && !data.reported)) return null
  return <CloudCardBody data={data} />
}

export function CloudCardBody({ data }: { data: AssetCloud }) {
  const it = data.instance
  const rep = data.reported
  if (!it && rep) {
    return (
      <SectionCard title="Cloud" icon={IconCloud} badge={<HowKnown by="agent" />}>
        <Stack gap="sm" data-testid="cloud-card-unread">
          <Text size={FS.meta}>
            Its agent says this is <Mono>{rep.id}</Mono>{rep.type ? <> ({rep.type})</> : null} in {providerName(rep.provider)}{' '}
            {rep.provider === 'aws' ? 'account' : 'project'} <Mono>{rep.account}</Mono>, {rep.zone || rep.region}.
          </Text>
          <Text size={FS.meta} c="dimmed">
            Argus does not read that {rep.provider === 'aws' ? 'account' : 'project'}, so it cannot show the instance's state, cost or
            changes. Connect it under <AnchorLink to="/cloud" size={FS.meta} c={INK.brand}>Cloud</AnchorLink>.
          </Text>
        </Stack>
      </SectionCard>
    )
  }
  if (!it) return null
  const r = it.resource
  const env = envOf(r.tags, it.environment)
  const by = it.server?.by ?? 'address'
  return (
    <SectionCard
      title="Cloud"
      icon={IconCloud}
      badge={<HowKnown by={by} />}
      action={
        <AnchorLink to="/cloud" search={{ tab: 'instances' }} size={FS.meta} fw={500} c={INK.brand}>Open in Cloud</AnchorLink>
      }
    >
      <Stack gap={8} data-testid="cloud-card">
        <Fact label="Instance"><Mono>{r.id}</Mono></Fact>
        <Fact label="State">
          <Group gap={6}>
            <Badge size="sm" variant="light" radius="sm" color={stateColor(r.state)}>{stateLabel(r.state)}</Badge>
            <Badge size="sm" variant="light" radius="sm" color={ENV_COLOR[env]}>{ENV_LABEL[env]}</Badge>
          </Group>
        </Fact>
        <Fact label="Where">
          <Text size={FS.meta}>{it.accountName} · {r.zone || r.region}</Text>
        </Fact>
        {r.size && <Fact label="Type"><Mono>{r.size}</Mono></Fact>}
        {(r.addresses?.length || r.publicIp) && (
          <Fact label="Addresses">
            <Mono>{[...(r.addresses ?? []), ...(r.publicIp ? [r.publicIp] : [])].join(' · ')}</Mono>
          </Fact>
        )}
        <Fact label="Read">
          <Text size={FS.meta} c="dimmed" title={absTime(it.seenAt)}>{relTime(it.seenAt)}, by Argus's last read of the account</Text>
        </Fact>
        {by !== 'agent' && (
          <Text size={FS.micro} c="dimmed">
            Matched by {by}, not reported by the agent. An agent update makes the link certain.
          </Text>
        )}
      </Stack>
    </SectionCard>
  )
}

/** How the link was made, so a match is never shown as a fact. */
function HowKnown({ by }: { by: 'agent' | 'address' | 'name' }) {
  return by === 'agent'
    ? <Badge size="xs" variant="outline" color="slate" radius="sm" tt="none">reported by the agent</Badge>
    : <Badge size="xs" variant="light" color="amber" radius="sm" tt="none">matched by {by}</Badge>
}

/** The instance type and cloud, for the asset's header; nothing while unknown. */
export function cloudHeaderLabel(data: AssetCloud | undefined): string | null {
  const it = data?.instance
  if (it) return [providerName(it.provider ?? 'aws'), it.accountName, it.resource.size].filter(Boolean).join(' · ')
  const rep = data?.reported
  if (rep) return [providerName(rep.provider), rep.type].filter(Boolean).join(' · ')
  return null
}

function providerName(p: string): string {
  return p === 'aws' ? 'AWS' : p === 'gcp' ? 'Google Cloud' : p
}

function Fact({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      <Text size={FS.meta} c="dimmed" w={90} style={{ flexShrink: 0 }}>{label}</Text>
      <Box style={{ minWidth: 0 }}>{children}</Box>
    </Group>
  )
}
