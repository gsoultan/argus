import { Alert, Badge, Box, Button, Card, Group, Select, Stack, Text } from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { IconAlertTriangle, IconShield } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { EmptyState } from '~/components/page'
import { portText, type CloudItem } from '~/lib/cloud'
import { buildSecurityMap, pathOf, reachOf, type MapNode, type SecurityMap } from '~/lib/securityMap'
import { FS } from '~/theme'
import { OriginBadge } from './common'

/*
Network › Firewalls › Map: who can reach in, through which security group,
to which instance, for one account and region at a time. Drawn only from
the security group rules in the last read, and it says so.
*/

const W = 210
const H = 50
const GAP = 66
const COL = [16, 380, 744]
const TOP = 44

interface Placed extends MapNode { x: number; y: number }

function place(map: SecurityMap): { nodes: Map<string, Placed>; height: number } {
  const nodes = new Map<string, Placed>()
  const cols = [map.sources, map.groups, map.instances]
  cols.forEach((list, c) => list.forEach((n, i) => nodes.set(n.key, { ...n, x: COL[c]!, y: TOP + i * GAP })))
  const rows = Math.max(1, ...cols.map((l) => l.length))
  return { nodes, height: TOP + rows * GAP + 10 }
}

const ink = {
  line: 'var(--mantine-color-gray-5)',
  risky: 'var(--mantine-color-rose-6)',
  sel: 'var(--mantine-color-azure-6, var(--mantine-color-blue-6))',
  border: 'var(--mantine-color-default-border)',
  text: 'var(--mantine-color-text)',
  dim: 'var(--mantine-color-dimmed)',
  bg: 'var(--mantine-color-body)',
}

function edgePath(a: Placed, b: Placed): { d: string; lx: number; ly: number } {
  if (a.kind === 'group' && b.kind === 'group') {
    // Group to group, in one column: bow out to the left of it.
    const x = a.x
    const y1 = a.y + H / 2
    const y2 = b.y + H / 2
    const bow = x - 70 - Math.min(60, Math.abs(y2 - y1) / 6)
    return { d: `M${x},${y1} C${bow},${y1} ${bow},${y2} ${x},${y2}`, lx: bow + 4, ly: (y1 + y2) / 2 }
  }
  const x1 = a.x + W
  const y1 = a.y + H / 2
  const x2 = b.x
  const y2 = b.y + H / 2
  const mx = (x1 + x2) / 2
  return { d: `M${x1},${y1} C${mx},${y1} ${mx},${y2} ${x2},${y2}`, lx: x1 + 14, ly: y1 - 6 }
}

function DesktopMap({ map, sel, setSel }: { map: SecurityMap; sel: string | null; setSel: (k: string | null) => void }) {
  const { nodes, height } = useMemo(() => place(map), [map])
  const keep = sel ? pathOf(map, sel) : null
  const faded = (k: string) => (keep && !keep.has(k) ? 0.25 : 1)
  return (
    <Box style={{ overflowX: 'auto' }}>
      <svg role="img" aria-label="Security group map" viewBox={`0 0 ${COL[2]! + W + 16} ${height}`} width="100%"
        style={{ minWidth: 760, display: 'block' }} onClick={() => setSel(null)}>
        <defs>
          {(['line', 'risky', 'sel'] as const).map((k) => (
            <marker key={k} id={`arrow-${k}`} viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto">
              <path d="M0,0L10,5L0,10z" fill={ink[k]} />
            </marker>
          ))}
        </defs>
        {['Who can reach in', 'Security groups', 'Instances'].map((t, i) => (
          <text key={t} x={COL[i]} y={20} fontSize={11} fontWeight={600} fill={ink.dim} letterSpacing=".05em">{t.toUpperCase()}</text>
        ))}
        {map.edges.map((e) => {
          const a = nodes.get(e.from)
          const b = nodes.get(e.to)
          if (!a || !b) return null
          const { d } = edgePath(a, b)
          const on = sel === e.key || (keep?.has(e.key) && sel !== null)
          const color = on ? 'sel' : e.risky && e.rules.length ? 'risky' : 'line'
          return (
            <g key={e.key} opacity={faded(e.key)} style={{ cursor: e.rules.length ? 'pointer' : 'default' }}
              onClick={(ev) => { ev.stopPropagation(); if (e.rules.length) setSel(e.key) }}>
              <path d={d} stroke="transparent" strokeWidth={14} fill="none" />
              <path d={d} stroke={ink[color]} strokeWidth={on ? 3 : 2} fill="none" markerEnd={`url(#arrow-${color})`} />
            </g>
          )
        })}
        {map.edges.filter((e) => e.label).map((e) => {
          const a = nodes.get(e.from)
          const b = nodes.get(e.to)
          if (!a || !b) return null
          const { lx, ly } = edgePath(a, b)
          // A label beside its source must fit before the next column; the
          // panel has the whole of it.
          const room = a.kind === 'group' && b.kind === 'group' ? 40 : 22
          const label = e.label.length > room ? e.label.slice(0, room - 1) + '…' : e.label
          const w = label.length * 6.2 + 12
          return (
            <g key={'l' + e.key} opacity={faded(e.key)} style={{ cursor: 'pointer' }} onClick={(ev) => { ev.stopPropagation(); setSel(e.key) }}>
              <rect x={lx} y={ly - 11} width={w} height={17} rx={4} fill={e.risky ? 'var(--mantine-color-rose-light)' : ink.bg}
                stroke={e.risky ? ink.risky : ink.border} />
              <text x={lx + 6} y={ly + 2} fontSize={10.5} fontFamily="var(--mantine-font-family-monospace)" fill={e.risky ? ink.risky : ink.text}>{label}</text>
            </g>
          )
        })}
        {[...nodes.values()].map((n) => {
          const selected = sel === n.key
          const stroke = selected ? ink.sel : n.kind === 'source' && n.risky ? ink.risky : n.kind === 'group' ? 'var(--mantine-color-blue-3)' : ink.border
          return (
            <g key={n.key} opacity={faded(n.key)} style={{ cursor: 'pointer' }} onClick={(ev) => { ev.stopPropagation(); setSel(n.key) }}>
              <rect x={n.x} y={n.y} width={W} height={H} rx={9} fill={ink.bg} stroke={stroke} strokeWidth={selected ? 2.5 : 1.5}
                strokeDasharray={n.kind === 'source' ? '4 3' : undefined} />
              <text x={n.x + 12} y={n.y + 21} fontSize={12.5} fontWeight={650} fill={ink.text}>{n.label.length > 22 ? n.label.slice(0, 21) + '…' : n.label}</text>
              <text x={n.x + 12} y={n.y + 38} fontSize={10.5} fontFamily="var(--mantine-font-family-monospace)" fill={ink.dim}>
                {n.meta.length > 30 ? n.meta.slice(0, 29) + '…' : n.meta}
              </text>
              {n.kind !== 'source' && (
                <text x={n.x + W - 10} y={n.y + 21} fontSize={9.5} fontWeight={600} textAnchor="end"
                  fill={n.madeByArgus ? 'var(--mantine-color-blue-6)' : 'var(--mantine-color-violet-6)'}>
                  {n.madeByArgus ? 'Argus' : 'existing'}
                </text>
              )}
            </g>
          )
        })}
      </svg>
    </Box>
  )
}

function Side({ map, sel, items, onOpen }: { map: SecurityMap; sel: string | null; items: Map<string, CloudItem>; onOpen: (it: CloudItem) => void }) {
  const edge = sel ? map.edges.find((e) => e.key === sel) : undefined
  const all = [...map.sources, ...map.groups, ...map.instances]
  const node = sel ? all.find((n) => n.key === sel) : undefined
  const name = (k: string) => all.find((n) => n.key === k)?.label ?? k
  if (edge) {
    const g = items.get(edge.ruleGroup ?? '')
    return (
      <Stack gap="sm">
        <Box>
          <Text size={FS.micro} c="dimmed">Path</Text>
          <Text fw={700}>{name(edge.from)} → {name(edge.to)}</Text>
          <Text size={FS.meta} c="dimmed">{edge.rules.length === 1 ? 'One rule allows it.' : `${edge.rules.length} rules allow it.`}</Text>
        </Box>
        {edge.rules.map((r, i) => (
          <Card key={i} withBorder padding="xs" radius="md">
            <Group gap={6}>
              <Badge size="sm" variant="light" color="blue" radius="sm">in</Badge>
              <Text size={FS.meta} fw={600}>{g?.resource.name}</Text>
              {g && <Box ml="auto"><OriginBadge it={g} size="xs" /></Box>}
            </Group>
            <Text size={FS.meta} ff="monospace" mt={6}>{portText(r)} from {r.source === '0.0.0.0/0' || r.source === '::/0' ? 'anywhere' : name(edge.from)}</Text>
            {r.description && <Text size={FS.micro} c="dimmed">“{r.description}”</Text>}
          </Card>
        ))}
        {edge.risky && (
          <Alert color="rose" variant="light" p="xs" icon={<IconAlertTriangle size={16} />}>
            <Text size={FS.meta}>Anyone on the internet can try this. Narrow it to a range you trust, or remove it.</Text>
          </Alert>
        )}
        <Text size={FS.meta} c="dimmed" lh={1.5}>
          Changing the rule is a change to {g?.resource.name}: Argus shows it as a plan first, and it waits for an approver where the
          environment asks for one. The group itself stays.
        </Text>
        {g && <Button size="xs" variant="default" leftSection={<IconShield size={14} />} w="fit-content" onClick={() => onOpen(g)}>Open {g.resource.name}</Button>}
      </Stack>
    )
  }
  if (node) {
    const it = items.get(node.id)
    const reach = node.kind === 'instance' ? reachOf(map, node.key) : []
    const into = map.edges.filter((e) => e.to === node.key && e.rules.length)
    return (
      <Stack gap="sm">
        <Box>
          <Text size={FS.micro} c="dimmed">{node.kind === 'source' ? 'Source' : node.kind === 'group' ? 'Security group' : 'Instance'}</Text>
          <Text fw={700}>{node.label}</Text>
          <Text size={FS.meta} c="dimmed" ff="monospace">{node.kind === 'source' ? node.meta : node.id}</Text>
        </Box>
        {it && <OriginBadge it={it} />}
        {node.kind !== 'source' && (
          <>
            <Text size={FS.meta} fw={600}>Who can reach it in</Text>
            {(node.kind === 'instance' ? reach.map((r) => ({ k: r.from + r.ports, from: r.from, ports: r.ports, risky: r.risky }))
              : into.map((e) => ({ k: e.key, from: name(e.from), ports: e.rules.map(portText).join(', '), risky: e.risky }))).map((r) => (
              <Group key={r.k} justify="space-between" wrap="nowrap">
                <Text size={FS.meta}>{r.from}</Text>
                <Text size={FS.meta} ff="monospace" c={r.risky ? 'rose' : undefined}>{r.ports}</Text>
              </Group>
            ))}
            {(node.kind === 'instance' ? reach.length : into.length) === 0 && <Text size={FS.meta} c="dimmed">Nothing, by its security group rules.</Text>}
          </>
        )}
        {it && <Button size="xs" variant="default" w="fit-content" onClick={() => onOpen(it)}>Open {node.label}</Button>}
      </Stack>
    )
  }
  return (
    <Stack gap="sm">
      <Box>
        <Text fw={700}>What this map is</Text>
        <Text size={FS.meta} c="dimmed" lh={1.5} mt={4}>
          Drawn only from the security group rules Argus last read. An arrow means an inbound rule lets that traffic in. Network
          ACLs and routes are not drawn, so a path here can still be blocked further along. Click an arrow or a box for the rules behind it.
        </Text>
      </Box>
      {map.risks.length > 0 ? (
        <Alert color="rose" variant="light" p="sm" title={`${map.risks.length} risky path${map.risks.length === 1 ? '' : 's'}`}>
          <Stack gap={4}>{map.risks.map((r) => <Text key={r} size={FS.meta}>{r}</Text>)}</Stack>
        </Alert>
      ) : (
        <Alert color="teal" variant="light" p="sm"><Text size={FS.meta}>Nothing risky is open to the internet here.</Text></Alert>
      )}
      <Stack gap={6}>
        <Group gap={8}><Box w={22} style={{ borderTop: `2px solid ${ink.line}` }} /><Text size={FS.micro} c="dimmed">allowed in</Text></Group>
        <Group gap={8}><Box w={22} style={{ borderTop: `2px solid ${ink.risky}` }} /><Text size={FS.micro} c="dimmed">risky from the internet</Text></Group>
        <Group gap={8}><Text size={FS.micro} fw={600} c="violet">existing</Text><Text size={FS.micro} c="dimmed">was in the account before Argus</Text></Group>
        <Group gap={8}><Text size={FS.micro} fw={600} c="blue">Argus</Text><Text size={FS.micro} c="dimmed">made by Argus</Text></Group>
      </Stack>
    </Stack>
  )
}

function PhoneMap({ map }: { map: SecurityMap }) {
  return (
    <Stack gap="sm">
      {map.risks.length > 0 && (
        <Alert color="rose" variant="light" p="sm" title={`${map.risks.length} risky path${map.risks.length === 1 ? '' : 's'}`}>
          <Stack gap={4}>{map.risks.map((r) => <Text key={r} size={FS.meta}>{r}</Text>)}</Stack>
        </Alert>
      )}
      {map.instances.map((n) => {
        const reach = reachOf(map, n.key)
        return (
          <Card key={n.key} withBorder padding="sm" radius="md">
            <Group justify="space-between"><Text fw={650}>{n.label}</Text>{reach.some((r) => r.risky) && <Badge color="rose" variant="light" radius="sm">risky</Badge>}</Group>
            <Text size={FS.micro} c="dimmed" mb={4}>Who can reach in</Text>
            {reach.length === 0 ? <Text size={FS.meta} c="dimmed">Nothing can reach it in</Text> : reach.map((r) => (
              <Group key={r.from + r.ports} justify="space-between" wrap="nowrap" py={2}>
                <Text size={FS.meta}>{r.from}</Text>
                <Text size={FS.meta} ff="monospace" c={r.risky ? 'rose' : undefined}>{r.ports}</Text>
              </Group>
            ))}
          </Card>
        )
      })}
    </Stack>
  )
}

/** The map, with its account and region picker. */
export function SecurityGroupMap({ firewalls, instances, onOpen }: {
  firewalls: CloudItem[]
  instances: CloudItem[]
  onOpen: (it: CloudItem) => void
}) {
  const phone = useMediaQuery('(max-width: 48em)') ?? false
  const scopes = useMemo(() => {
    const seen = new Map<string, { value: string; label: string; n: number }>()
    for (const f of firewalls) {
      const k = `${f.accountId}|${f.resource.region}`
      const s = seen.get(k) ?? { value: k, label: `${f.accountName} · ${f.resource.region}`, n: 0 }
      s.n++
      seen.set(k, s)
    }
    return [...seen.values()].sort((a, b) => b.n - a.n)
  }, [firewalls])
  const [scope, setScope] = useState<string | null>(null)
  const [sel, setSel] = useState<string | null>(null)
  const current = scope && scopes.some((s) => s.value === scope) ? scope : scopes[0]?.value ?? null
  const [acct, region] = current ? current.split('|') : ['', '']
  const map = useMemo(() => buildSecurityMap(firewalls, instances, acct!, region!), [firewalls, instances, acct, region])
  const items = useMemo(() => {
    const m = new Map<string, CloudItem>()
    for (const it of [...firewalls, ...instances]) if (it.accountId === acct) m.set(it.resource.id, it)
    return m
  }, [firewalls, instances, acct])
  if (!current) {
    return <EmptyState icon={IconShield} title="Nothing to draw" description="No security groups match the account or search." />
  }
  return (
    <Stack gap="sm">
      {scopes.length > 1 && (
        <Select aria-label="Account and region" w={320} value={current} allowDeselect={false}
          data={scopes.map(({ value, label }) => ({ value, label }))} onChange={(v) => { setScope(v); setSel(null) }} />
      )}
      {phone ? <PhoneMap map={map} /> : (
        <Card withBorder padding={0} radius="md">
          <Box style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 1fr) 340px' }}>
            <Box p="md"><DesktopMap map={map} sel={sel} setSel={setSel} /></Box>
            <Box p="md" style={{ borderLeft: `1px solid ${ink.border}` }}>
              <Side map={map} sel={sel} items={items} onOpen={onOpen} />
            </Box>
          </Box>
        </Card>
      )}
    </Stack>
  )
}
