import { Badge, Box, Code, Drawer, Group, Stack, Table, Text, Tooltip } from '@mantine/core'
import { IconAlertTriangle, IconServer2 } from '@tabler/icons-react'
import { AnchorLink } from '~/components/links'
import type { ReactNode } from 'react'
import { Field, relTime } from '~/components/primitives'
import {
  ENV_COLOR, ENV_LABEL, openToInternet, portText, ruleExposure, stateColor, stateLabel,
  type CloudEnvironment, type CloudItem, type CloudResource,
} from '~/lib/cloud'
import { FS, INK, SP } from '~/theme'
import { InstanceActions } from './InstanceActions'
import { InstanceSchedule } from './Schedules'
import { FirewallActions, TargetActions } from './NetworkActions'
import { StorageActions } from './StorageActions'

export function EnvBadge({ env }: { env: CloudEnvironment }) {
  return <Badge variant="light" color={ENV_COLOR[env]} radius="sm">{ENV_LABEL[env]}</Badge>
}

export function StateBadge({ state }: { state?: string }) {
  const c = stateColor(state)
  return (
    <Badge variant="light" color={c} radius="sm" leftSection={c === 'teal' || c === 'sky' ? <Dot /> : undefined}>
      {stateLabel(state)}
    </Badge>
  )
}

function Dot() {
  return <Box component="span" w={6} h={6} style={{ borderRadius: '50%', background: 'currentColor', display: 'inline-block' }} />
}

/**
 * Whether Argus made it or it was already in the account. Only what Argus
 * made can be removed or deleted from Argus; the rest is managed, never
 * deleted.
 */
export function OriginBadge({ it, size = 'sm' }: { it: CloudItem; size?: 'xs' | 'sm' }) {
  return it.madeByArgus
    ? <Tooltip label="Argus launched or created it, by its own record"><Badge size={size} variant="light" color="blue" radius="sm" tt="none">Made by Argus</Badge></Tooltip>
    : (
      <Tooltip label="It was in the account before Argus, or Argus has no record of making it. Argus manages it but never removes or deletes it." multiline w={260}>
        <Badge size={size} variant="light" color="violet" radius="sm" tt="none">Already in the account</Badge>
      </Tooltip>
    )
}

/** The resource's name, with its id beneath when they differ. */
export function NameCell({ r }: { r: CloudResource }) {
  return (
    <Box style={{ minWidth: 0 }}>
      <Text fw={600} size={FS.body} truncate>{r.name}</Text>
      {r.name !== r.id && <Text size={FS.micro} c="dimmed" ff="monospace" truncate>{r.id}</Text>}
    </Box>
  )
}

/** Where it is: account, then zone or region. */
export function WhereCell({ it }: { it: CloudItem }) {
  return (
    <Box>
      <Text size={FS.meta}>{it.accountName}</Text>
      <Text size={FS.micro} c="dimmed">{it.resource.zone || it.resource.region}</Text>
    </Box>
  )
}

/**
 * The Argus server an instance appears to be. It is a match by address or
 * name, not something an agent reported, and the tooltip says which.
 */
export function ServerCell({ it }: { it: CloudItem }) {
  if (!it.server) {
    return (
      <Tooltip label="No Argus server has this instance's address or name. Sessions on it are not recorded by Argus.">
        <Badge variant="light" color="amber" radius="sm" leftSection={<IconAlertTriangle size={11} />}>No Argus server</Badge>
      </Tooltip>
    )
  }
  return (
    <Tooltip label={`Matched by ${it.server.by === 'address' ? 'address' : 'name'}`}>
      <Box component="span" onClick={(e: React.MouseEvent) => e.stopPropagation()}>
        <AnchorLink to="/assets/$assetId" params={{ assetId: it.server.id }} size={FS.meta} c={INK.brand} ff="monospace">
          <Group gap={4} wrap="nowrap" component="span"><IconServer2 size={13} />{it.server.hostname}</Group>
        </AnchorLink>
      </Box>
    </Tooltip>
  )
}

/** One resource in full, in a side panel. */
export function ResourceDrawer({ it, onClose, names }: {
  it: CloudItem | null
  onClose: () => void
  /** Names of other resources, by id, so links read as names. */
  names: Map<string, string>
}) {
  const r = it?.resource
  return (
    <Drawer opened={Boolean(it)} onClose={onClose} position="right" size="md" title={r ? <Text fw={700} size="lg">{r.name}</Text> : null}>
      {it && r && (
        <Stack gap="lg">
          <Group gap="xs">
            {r.state && <StateBadge state={r.state} />}
            <EnvBadge env={it.environment} />
            {(r.kind === 'instance' || r.kind === 'firewall') && <OriginBadge it={it} />}
            {openToInternet(r).length > 0 && (
              <Badge variant="light" color="rose" radius="sm">Open to the internet: {openToInternet(r).join(', ')}</Badge>
            )}
          </Group>
          {r.kind === 'instance' && <InstanceActions it={it} />}
          {r.kind === 'instance' && <InstanceSchedule it={it} />}
          {(r.kind === 'volume' || r.kind === 'snapshot' || r.kind === 'image' || r.kind === 'address') && <StorageActions it={it} />}
          {r.kind === 'firewall' && <FirewallActions it={it} />}
          {r.kind === 'target-group' && <TargetActions it={it} />}
          <Box style={{ display: 'grid', gridTemplateColumns: '1fr 1fr', gap: 16 }}>
            <Field label="Id"><Text size={FS.meta} ff="monospace" style={{ overflowWrap: 'anywhere' }}>{r.id}</Text></Field>
            <Field label="Account"><Text size={FS.meta}>{it.accountName}</Text></Field>
            <Field label="Where"><Text size={FS.meta}>{r.zone || r.region}</Text></Field>
            {r.size && <Field label={r.kind === 'instance' ? 'Type' : 'Size'}><Text size={FS.meta} ff="monospace">{r.size}</Text></Field>}
            {(r.addresses?.length ?? 0) > 0 && <Field label="Private address"><Text size={FS.meta} ff="monospace">{r.addresses!.join(', ')}</Text></Field>}
            {r.publicIp && <Field label="Public address"><Text size={FS.meta} ff="monospace">{r.publicIp}</Text></Field>}
            {r.network && <Field label="Network"><Text size={FS.meta} ff="monospace">{names.get(r.network) ?? r.network}</Text></Field>}
            {r.createdAt && <Field label="Created"><Text size={FS.meta}>{relTime(r.createdAt)}</Text></Field>}
            <Field label="First seen by Argus"><Text size={FS.meta}>{relTime(it.firstSeen)}</Text></Field>
            <Field label="Last read"><Text size={FS.meta}>{relTime(it.seenAt)}</Text></Field>
          </Box>
          {it.resource.kind === 'instance' && (
            <Field label="Argus server"><ServerCell it={it} /></Field>
          )}
          {(r.links?.length ?? 0) > 0 && (
            <Field label="Uses">
              <Group gap={6} mt={4}>
                {r.links!.map((l) => (
                  <Badge key={l.kind + l.id} variant="default" radius="sm" tt="none" fw={500}>
                    {LINK_LABEL[l.kind] ?? l.kind}: {l.name && l.kind === 'firewall' ? l.name : names.get(l.id) ?? l.id}
                  </Badge>
                ))}
              </Group>
            </Field>
          )}
          {(r.rules?.length ?? 0) > 0 && <RulesTable r={r} />}
          {r.tags && Object.keys(r.tags).length > 0 && (
            <Field label="Tags"><KeyValues m={r.tags} /></Field>
          )}
          {r.details && Object.keys(r.details).length > 0 && (
            <Field label="AWS details"><KeyValues m={r.details} /></Field>
          )}
        </Stack>
      )}
    </Drawer>
  )
}

const LINK_LABEL: Record<string, string> = {
  firewall: 'Firewall', subnet: 'Subnet', volume: 'Disk', instance: 'Instance', network: 'Network', snapshot: 'Snapshot', image: 'Image',
  'load-balancer': 'Load balancer', 'target-group': 'Target group',
}

function KeyValues({ m }: { m: Record<string, string> }) {
  return (
    <Table mt={4} withRowBorders={false} verticalSpacing={2}>
      <Table.Tbody>
        {Object.entries(m).sort(([a], [b]) => a.localeCompare(b)).map(([k, v]) => (
          <Table.Tr key={k}>
            <Table.Td w="40%" style={{ verticalAlign: 'top' }}><Text size={FS.meta} c="dimmed">{k}</Text></Table.Td>
            <Table.Td><Text size={FS.meta} ff="monospace" style={{ overflowWrap: 'anywhere' }}>{v}</Text></Table.Td>
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  )
}

function RulesTable({ r }: { r: CloudResource }) {
  const rows = (dir: boolean) => (r.rules ?? []).filter((x) => x.inbound === dir)
  const block = (title: string, list: NonNullable<CloudResource['rules']>): ReactNode => (
    <Field label={title}>
      {list.length === 0 ? <Text size={FS.meta} c="dimmed">None</Text> : (
        <Stack gap={SP.tight} mt={4}>
          {list.map((x, i) => {
            const open = x.source === '0.0.0.0/0' || x.source === '::/0'
            const risky = ruleExposure(x) !== ''
            return (
              <Group key={i} gap="xs" wrap="nowrap" justify="space-between">
                <Code fz={FS.meta} c={risky ? INK.denied : undefined}>{portText(x)}</Code>
                <Text size={FS.meta} ff="monospace" c={risky ? INK.denied : 'dimmed'} truncate>
                  {x.inbound ? 'from' : 'to'} {open ? 'anywhere' : x.source}
                </Text>
              </Group>
            )
          })}
        </Stack>
      )}
    </Field>
  )
  return (
    <>
      {block('Inbound rules', rows(true))}
      {block('Outbound rules', rows(false))}
    </>
  )
}
