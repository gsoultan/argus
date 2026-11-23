import { Alert, Badge, Box, Card, Code, Group, Skeleton, Stack, Table, Text, ThemeIcon } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { AnchorLink } from '~/components/links'
import { IconShieldCheck, IconShieldOff, IconShieldQuestion } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import {
  firewallName, ruleWhat, rulePorts, sourceName,
  type FirewallListen, type ServerFirewall, type ServerFirewallRule,
} from '~/lib/firewallApi'
import { assetFirewallQuery } from '~/lib/firewallQueries'
import { isBroker } from '~/lib/engines'
import { isConfigured } from '~/lib/live'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * The firewall on a server, as its agent last read it: whether it is on,
 * what happens to traffic no rule names, every rule in plain words with who
 * made it, and who can reach each program waiting for connections.
 *
 * Read only. Changing it from here is the next step of the plan
 * (plans/server-firewall/design.md); until then nothing on this tab offers
 * to change anything, so nothing looks set that is not.
 */
export function FirewallTab({ asset }: { asset: Asset }) {
  const { data: v, isLoading, error } = useQuery(assetFirewallQuery(asset.id))

  if (!isConfigured()) {
    return (
      <Card padding="md">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          A server&apos;s firewall is read by its agent, through a control plane. Connect this console to Argus to see it.
        </Text>
      </Card>
    )
  }
  if (error) {
    return (
      <Alert color="rose" title="The firewall could not be read">
        <Text size={FS.body}>{error instanceof Error ? error.message : String(error)}</Text>
      </Alert>
    )
  }
  if (isLoading || !v) return <Skeleton h={240} />

  if (!v.reported) {
    return (
      <Card padding="md" data-testid="firewall-not-reported">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          {asset.agentState === 'absent'
            ? 'No agent is enrolled on this server. Install one from Servers, Add a server, and its firewall appears here.'
            : "This server's agent has not reported its firewall yet. An agent older than this console does not read it: update the agent from the Agent tab, and the firewall appears here within ten minutes."}
        </Text>
      </Card>
    )
  }

  const fw = v.firewall
  const incoming = v.rules.filter((r) => r.direction === 'in')
  const outgoing = v.rules.filter((r) => r.direction === 'out')
  return (
    <Stack gap="md">
      <StatusCard view={v} />
      {fw.error ? (
        <Alert color="amber" title="The agent could not read the firewall">
          <Text size={FS.body}>{fw.error}</Text>
        </Alert>
      ) : (
        <>
          {fw.docker && (
            <Alert color="amber" title="Docker opens its own ports" data-testid="firewall-docker">
              <Text size={FS.body} lh={1.5}>
                Ports a container publishes go around {firewallName(fw.kind)}, so they can be reachable even when no
                rule below lets them in. Check them under Apps, Containers.
              </Text>
            </Alert>
          )}
          {fw.profiles && fw.profiles.length > 0 && <ProfilesCard view={v} />}
          {(fw.active || incoming.length > 0) && (
            <RulesCard
              title="What can come in"
              description="Each line lets one kind of traffic in, or keeps it out. Names are servers in Argus."
              rules={incoming}
              names={v.names}
              footer={defaultLine(fw.active, fw.defaultIn, 'in')}
              hidden={fw.hidden}
              testId="firewall-in"
            />
          )}
          {outgoing.length > 0 && (
            <RulesCard
              title="What can go out"
              description="Rules for traffic leaving this server."
              rules={outgoing}
              names={v.names}
              footer={defaultLine(fw.active, fw.defaultOut, 'out')}
              testId="firewall-out"
            />
          )}
          <ListeningCard view={v} />
        </>
      )}
      {fw.note && (
        <Text size={FS.meta} c="dimmed">{fw.note}</Text>
      )}
    </Stack>
  )
}

/* ── State ───────────────────────────────────────────────────────────────── */

function StatusCard({ view }: { view: ServerFirewall }) {
  const fw = view.firewall
  const on = fw.active && !fw.error
  const none = !fw.kind && !fw.error
  const color = fw.error ? 'slate' : on ? 'teal' : 'rose'
  const Icon = fw.error ? IconShieldQuestion : on ? IconShieldCheck : IconShieldOff
  const title = fw.error ? 'Firewall not read' : none ? 'No firewall installed' : on ? 'Firewall is on' : 'Firewall is off'
  const what = none
    ? 'Anything listening on this server can be reached by anyone who can reach the server.'
    : on
      ? fw.defaultIn === 'allow'
        ? 'lets in everything no rule blocks'
        : 'blocks what is not let in below'
      : 'it is installed but does not filter anything; whatever listens can be reached'
  return (
    <Card padding="md" data-testid="firewall-status">
      <Group gap={SP.cozy} wrap="nowrap" align="flex-start">
        <ThemeIcon size={38} radius="md" variant="light" color={color}>
          <Icon size={22} />
        </ThemeIcon>
        <Box style={{ flex: 1, minWidth: 0 }}>
          <Group gap={SP.snug}>
            <Text fw={650} size="md">{title}</Text>
            <Badge size="sm" variant="light" color="slate" radius="sm" tt="none" title="Changing it from Argus comes next">
              Read only for now
            </Badge>
          </Group>
          <Text size={FS.meta} c="dimmed" mt={2} lh={1.5}>
            {fw.kind ? `${firewallName(fw.kind)} · ` : ''}
            {what}
            {fw.zones && fw.zones.length > 0 ? ` · zones ${fw.zones.join(', ')}` : ''}
            {' · '}
            <span title={absTime(fw.readAt)}>read by the agent {relTime(fw.readAt)}</span>
          </Text>
          {!on && !none && fw.installed && fw.installed.length > 1 && (
            <Text size={FS.meta} c="dimmed" mt={2}>Installed: {fw.installed.map(firewallName).join(', ')}.</Text>
          )}
        </Box>
      </Group>
    </Card>
  )
}

function ProfilesCard({ view }: { view: ServerFirewall }) {
  const profiles = view.firewall.profiles ?? []
  return (
    <Card padding="sm" data-testid="firewall-profiles">
      <Group gap="lg" wrap="wrap">
        <Text size={FS.meta} c="dimmed">Network types</Text>
        {profiles.map((p) => (
          <Group key={p.name} gap={SP.tight} wrap="nowrap">
            <Text size="sm" fw={600}>{p.name}</Text>
            <Badge size="sm" variant="light" radius="sm" tt="none" color={p.enabled ? 'teal' : 'rose'}>
              {p.enabled ? 'On' : 'Off'}
            </Badge>
            {p.inUse && <Text size={FS.meta} c="dimmed">in use</Text>}
          </Group>
        ))}
        <Text size={FS.meta} c="dimmed" ml="auto">Windows picks one by the network the server is plugged into.</Text>
      </Group>
    </Card>
  )
}

function defaultLine(active: boolean, v: string | undefined, dir: 'in' | 'out'): string {
  const what = dir === 'in' ? 'Everything else coming in' : 'Everything else going out'
  if (!active) return `The firewall is off: ${what.toLowerCase()} is allowed.`
  if (v === 'allow') return `${what} is allowed.`
  if (v === 'deny' || v === 'reject') return `${what} is blocked.`
  return `${what}: the firewall did not say.`
}

/* ── Rules ───────────────────────────────────────────────────────────────── */

function RulesCard({ title, description, rules, names, footer, hidden, testId }: {
  title: string
  description: string
  rules: ServerFirewallRule[]
  names: Record<string, string>
  footer: string
  hidden?: number
  testId: string
}) {
  return (
    <SectionCard title={title} description={description} flush>
      <Box data-testid={testId}>
        <Table verticalSpacing="sm" horizontalSpacing="md">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Lets in</Table.Th>
              <Table.Th>From</Table.Th>
              <Table.Th>Who made it</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {rules.length === 0 && (
              <Table.Tr>
                <Table.Td colSpan={3}>
                  <Text size={FS.body} c="dimmed">No rules.</Text>
                </Table.Td>
              </Table.Tr>
            )}
            {rules.map((r, i) => (
              <RuleRow key={i} rule={r} names={names} />
            ))}
            <Table.Tr>
              <Table.Td colSpan={3} bg="var(--mantine-color-body)">
                <Text size={FS.meta} c="dimmed">
                  {footer}
                  {hidden ? ` ${hidden} more ${hidden === 1 ? 'rule is' : 'rules are'} off and not shown.` : ''}
                </Text>
              </Table.Td>
            </Table.Tr>
          </Table.Tbody>
        </Table>
      </Box>
    </SectionCard>
  )
}

function RuleRow({ rule: r, names }: { rule: ServerFirewallRule; names: Record<string, string> }) {
  if (r.action === 'other') {
    return (
      <Table.Tr>
        <Table.Td colSpan={2}>
          <Text size="sm">A rule Argus cannot put in words</Text>
          <Code fz={FS.micro}>{r.raw}</Code>
        </Table.Td>
        <Table.Td><MadeBy rule={r} /></Table.Td>
      </Table.Tr>
    )
  }
  const blocks = r.action === 'deny' || r.action === 'reject'
  const ports = rulePorts(r)
  const sources = r.sources ?? []
  const open = !blocks && sources.length === 0
  return (
    <Table.Tr>
      <Table.Td>
        <Group gap={SP.tight} wrap="nowrap">
          {blocks && <Badge size="xs" variant="light" color="rose" radius="sm" tt="none">Blocks</Badge>}
          <Text size="sm" fw={600}>{ruleWhat(r)}</Text>
          {ports && <Text size={FS.meta} c="dimmed" ff="monospace">{ports}</Text>}
          {r.ipv6 && <Text size={FS.micro} c="dimmed">IPv6</Text>}
        </Group>
      </Table.Td>
      <Table.Td>
        {sources.length === 0 ? (
          <Group gap={SP.tight}>
            <Text size="sm">Everyone</Text>
            {open && r.direction === 'in' && (
              <Badge size="xs" variant="light" color="amber" radius="sm" tt="none">The internet included</Badge>
            )}
          </Group>
        ) : (
          <Text size="sm">
            {sources.map((s, i) => (
              <span key={s}>
                {i > 0 && ', '}
                {names[s] ? <b>{names[s]}</b> : <Text span size="sm" ff="monospace">{s}</Text>}
              </span>
            ))}
          </Text>
        )}
      </Table.Td>
      <Table.Td><MadeBy rule={r} /></Table.Td>
    </Table.Tr>
  )
}

function MadeBy({ rule: r }: { rule: ServerFirewallRule }) {
  if (r.madeBy === 'database' && r.database) {
    return (
      <Group gap={SP.tight} wrap="nowrap">
        <Badge size="xs" variant="light" color="azure" radius="sm" tt="none">Argus</Badge>
        <Text size={FS.meta} c="dimmed">
          <AnchorLink
            {...(isBroker(r.database.engine)
              ? ({ to: '/brokers/$brokerId', params: { brokerId: r.database.id } } as const)
              : ({ to: '/databases/$dbId', params: { dbId: r.database.id } } as const))}
            size={FS.meta}
          >
            {r.database.name}
          </AnchorLink>
          {' '}› Who can connect
        </Text>
      </Group>
    )
  }
  if (r.madeBy === 'argus') {
    return (
      <Group gap={SP.tight} wrap="nowrap">
        <Badge size="xs" variant="light" color="azure" radius="sm" tt="none">Argus</Badge>
        {r.argusPort ? <Text size={FS.meta} c="dimmed">for port {r.argusPort}</Text> : null}
      </Group>
    )
  }
  return (
    <Group gap={SP.tight} wrap="nowrap">
      <Badge size="xs" variant="light" color="slate" radius="sm" tt="none">By hand</Badge>
      <Text size={FS.meta} c="dimmed" lineClamp={1} title={r.name || r.raw}>{r.name || 'made outside Argus'}</Text>
    </Group>
  )
}

/* ── Listening ───────────────────────────────────────────────────────────── */

function ListeningCard({ view }: { view: ServerFirewall }) {
  if (!view.listeningRead) {
    return (
      <Card padding="md">
        <Text size={FS.body} c="dimmed" lh={1.5}>
          {view.firewall.kind === 'windows'
            ? 'Which programs wait for connections is not read on Windows yet, so who can reach each one is not shown.'
            : 'The agent has not said which programs wait for connections yet, so who can reach each one is not shown.'}
        </Text>
      </Card>
    )
  }
  const open = view.listening.filter((l) => !l.localOnly)
  const local = view.listening.length - open.length
  return (
    <SectionCard
      title="Programs waiting for connections"
      description="What the agent sees listening, and who the firewall lets reach it."
      flush
    >
      <Box data-testid="firewall-listening">
        <Table verticalSpacing="sm" horizontalSpacing="md">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Program</Table.Th>
              <Table.Th>Port</Table.Th>
              <Table.Th>Who can reach it</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {open.length === 0 && (
              <Table.Tr>
                <Table.Td colSpan={3}>
                  <Text size={FS.body} c="dimmed">Nothing listens for connections from other machines.</Text>
                </Table.Td>
              </Table.Tr>
            )}
            {open.map((l) => (
              <Table.Tr key={l.port}>
                <Table.Td><Text size="sm">{l.process || l.kind || 'unknown'}</Text></Table.Td>
                <Table.Td><Text size="sm" ff="monospace">{l.port}</Text></Table.Td>
                <Table.Td><ReachText listen={l} names={view.names} /></Table.Td>
              </Table.Tr>
            ))}
            {local > 0 && (
              <Table.Tr>
                <Table.Td colSpan={3}>
                  <Text size={FS.meta} c="dimmed">
                    {local} more {local === 1 ? 'listens' : 'listen'} only on this server itself, which nothing else can reach.
                  </Text>
                </Table.Td>
              </Table.Tr>
            )}
          </Table.Tbody>
        </Table>
      </Box>
    </SectionCard>
  )
}

/** Who can reach a port, in words, for the Listening table. */
export function reachWords(l: Pick<FirewallListen, 'reach' | 'localOnly'>, names: Record<string, string>): {
  text: string
  color?: string
} {
  if (l.localOnly) return { text: 'Only this server' }
  const r = l.reach
  if (r.unknown) return { text: 'Not known', color: 'slate' }
  if (r.everyone) return { text: 'Everyone, the internet included', color: 'amber' }
  const srcs = r.sources ?? []
  if (srcs.length === 0) return { text: 'Nobody from outside', color: 'slate' }
  const text = srcs.map((s) => sourceName(s, names)).join(', ')
  return r.public ? { text: `${text} (on the internet)`, color: 'amber' } : { text }
}

function ReachText({ listen, names }: { listen: FirewallListen; names: Record<string, string> }) {
  const w = reachWords(listen, names)
  if (!w.color) return <Text size="sm">{w.text}</Text>
  return (
    <Badge size="sm" variant="light" color={w.color} radius="sm" tt="none" style={{ whiteSpace: 'normal', height: 'auto' }}>
      {w.text}
    </Badge>
  )
}
