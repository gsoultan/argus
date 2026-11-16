import { Badge, Box, Button, Group, Modal, Select, Stack, Table, Text, Tooltip } from '@mantine/core'
import { useElementSize } from '@mantine/hooks'
import { useNavigate } from '@tanstack/react-router'
import { IconStack3 } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { DataTable, EmptyState, SectionCard } from '~/components/page'
import { relTime, rowNav } from '~/components/primitives'
import { money, type CloudItem } from '~/lib/cloud'
import { MODE_WORDS, groupNow, serving, type ScaleGroup, type ScalePlan, type ScalePoint } from '~/lib/cloudScale'
import { FS } from '~/theme'

/*
 * Scale groups on Cloud › Scaling (mockup 05), the group chart its page and
 * an Argus question share (mockups 11 and 12), and how a question's plan
 * reads in Approvals.
 */

/** What changed last, in a line: "Asked 14:02, waiting", "Added web-03 yesterday". */
function lastWords(g: ScaleGroup): string {
  if (g.open?.state === 'awaiting_approval') return `Asked ${relTime(g.open.requestedAt)}, waiting`
  if (!g.last) return `Made ${relTime(g.createdAt)}`
  return `${g.last.detail.split(';')[0]} · ${relTime(g.last.at)}`
}

export function ScaleGroupsCard({ groups, loading, instances, canMake }: {
  groups?: ScaleGroup[]; loading?: boolean; instances?: CloudItem[]; canMake: boolean
}) {
  const navigate = useNavigate()
  const [picking, setPicking] = useState(false)
  return (
    <SectionCard title="Scale groups" icon={IconStack3} flush
      description="Servers that do the same job behind one load balancer. Argus adds one when they are busy and takes one away when they are quiet, by the group's rules."
      action={canMake ? <Button size="xs" onClick={() => setPicking(true)}>New scale group</Button> : undefined}>
      <DataTable minWidth={760} loading={loading} isEmpty={(groups?.length ?? 0) === 0}
        empty={<EmptyState icon={IconStack3} title="No scale groups yet"
          description="Start one from a server that is busy: it is copied, and new servers like it join its load balancer when the rules say so." />}
        columns={['Group', 'Account', 'Servers', 'Who decides', 'Now', 'Last change']}>
        {groups?.map((g) => {
          const now = groupNow(g)
          return (
            <Table.Tr key={g.id} {...rowNav(() => void navigate({ to: '/cloud/scale/$groupId', params: { groupId: g.id } }))}>
              <Table.Td>
                <Text fw={600} size={FS.body}>{g.name}</Text>
                <Text size={FS.micro} c="dimmed">Behind {g.balancer || g.targetName}</Text>
              </Table.Td>
              <Table.Td><Text size={FS.meta}>{g.accountName}</Text><Text size={FS.micro} c="dimmed">{g.region}</Text></Table.Td>
              <Table.Td><Text size={FS.meta}><b>{serving(g).length}</b> of {g.limits.min}–{g.limits.max}</Text></Table.Td>
              <Table.Td>
                {g.state === 'paused'
                  ? <Tooltip label={g.pausedReason} multiline w={280}><Badge variant="light" color="slate" radius="sm">Paused</Badge></Tooltip>
                  : <Text size={FS.meta}>{MODE_WORDS[g.mode]}</Text>}
              </Table.Td>
              <Table.Td>{now ? <Badge variant="light" color={now.color} radius="sm">{now.label}</Badge> : <Text size={FS.micro} c="dimmed">No metrics</Text>}</Table.Td>
              <Table.Td maw={240}><Text size={FS.meta} c="dimmed" lineClamp={2}>{lastWords(g)}</Text></Table.Td>
            </Table.Tr>
          )
        })}
      </DataTable>
      {picking && <PickServer instances={instances} onClose={() => setPicking(false)} />}
    </SectionCard>
  )
}

/** A group starts from a server: pick which. */
function PickServer({ instances, onClose }: { instances?: CloudItem[]; onClose: () => void }) {
  const navigate = useNavigate()
  const [pick, setPick] = useState<string | null>(null)
  const running = (instances ?? []).filter((i) => i.resource.state === 'running')
  const data = running.map((i) => ({ value: `${i.accountId}|${i.resource.region}|${i.resource.id}`, label: `${i.resource.name} · ${i.resource.size} · ${i.accountName}` }))
  return (
    <Modal opened onClose={onClose} title="Start a scale group from a server" size="md">
      <Stack gap="sm">
        <Text size={FS.body}>
          A group is made from a server that already does the job: Argus copies it, and new servers like it join its load balancer.
        </Text>
        <Select label="Server" placeholder={data.length ? 'Pick a running server' : 'No running servers'} data={data} value={pick} onChange={setPick} searchable />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!pick} onClick={() => {
            const [account, region, from] = (pick ?? '').split('|')
            void navigate({ to: '/cloud/scale/new', search: { account, region, from } })
          }}>Next</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

const MEMBER = 'var(--mantine-color-azure-3)'
const AVERAGE = 'var(--mantine-color-text)'

/**
 * Processor across a group: the average as a dark line, each member as a
 * thin one, the rule's thresholds dashed, members joining as markers. A
 * stretch with no report is a gap, never joined across.
 */
export function GroupChart({ series, average, add, remove, joins, hours = 8, height = 260 }: {
  series?: Record<string, ScalePoint[]>
  average?: ScalePoint[]
  add?: number
  remove?: number
  joins?: { at: string; label: string }[]
  hours?: number
  height?: number
}) {
  const { ref, width } = useElementSize()
  const W = Math.max(260, Math.round(width) || 800)
  const H = height
  const pl = 36
  const pr = 8
  const pt = 8
  const pb = 20
  const iw = W - pl - pr
  const ih = H - pt - pb
  const end = Date.now()
  const start = end - hours * 3600_000
  const x = (at: string) => pl + (iw * (new Date(at).getTime() - start)) / (end - start)
  const y = (v: number) => pt + ih - (ih * Math.max(0, Math.min(100, v))) / 100
  const line = (pts: ScalePoint[]) => {
    const segs: string[][] = [[]]
    let prev = 0
    for (const p of pts) {
      const t = new Date(p.at).getTime()
      if (t < start) continue
      if (prev && t - prev > 11 * 60_000) segs.push([])
      segs[segs.length - 1]!.push(`${x(p.at).toFixed(1)},${y(p.value).toFixed(1)}`)
      prev = t
    }
    return segs.filter((s) => s.length > 1)
  }
  const avg = useMemo(() => average ?? averageOf(series ?? {}), [average, series])
  const members = Object.entries(series ?? {})
  const empty = avg.length < 2
  return (
    <Box ref={ref}>
      <Group gap="md" mb={6} justify="flex-end">
        <Group gap={6}><svg width="16" height="4" aria-hidden><line x1="0" y1="2" x2="16" y2="2" stroke={AVERAGE} strokeWidth="3" /></svg><Text size={FS.micro} c="dimmed">group average</Text></Group>
        {members.length > 0 && <Group gap={6}><svg width="16" height="4" aria-hidden><line x1="0" y1="2" x2="16" y2="2" stroke={MEMBER} strokeWidth="2" /></svg><Text size={FS.micro} c="dimmed">each member</Text></Group>}
      </Group>
      <svg viewBox={`0 0 ${W} ${H}`} width="100%" height={H} role="img" aria-label={`Processor across the group over the last ${hours} hours`}>
        {[0, 25, 50, 75, 100].map((v) => (
          <g key={v}>
            <line x1={pl} x2={W - pr} y1={y(v)} y2={y(v)} stroke="var(--color-line)" vectorEffect="non-scaling-stroke" />
            <text x={pl - 6} y={y(v) + 4} fontSize="10" textAnchor="end" fill="var(--mantine-color-dimmed)">{v}%</text>
          </g>
        ))}
        {[0, 0.5, 1].map((f) => (
          <text key={f} x={pl + iw * f} y={H - 4} fontSize="10" textAnchor={f === 0 ? 'start' : f === 1 ? 'end' : 'middle'} fill="var(--mantine-color-dimmed)">
            {f === 1 ? 'now' : `-${Math.round(hours * (1 - f))}h`}
          </text>
        ))}
        {members.length > 1 && members.flatMap(([h, pts]) => line(pts).map((seg, i) => (
          <polyline key={h + i} points={seg.join(' ')} fill="none" stroke={MEMBER} strokeWidth="1.2" vectorEffect="non-scaling-stroke" />
        )))}
        {line(avg).map((seg, i) => (
          <polyline key={'avg' + i} points={seg.join(' ')} fill="none" stroke={AVERAGE} strokeWidth="2.2" strokeLinejoin="round" vectorEffect="non-scaling-stroke" />
        ))}
        {add != null && (
          <g>
            <line x1={pl} x2={W - pr} y1={y(add)} y2={y(add)} stroke="var(--mantine-color-amber-7)" strokeDasharray="5 4" vectorEffect="non-scaling-stroke" />
            <text x={W - pr} y={y(add) - 4} fontSize="10" textAnchor="end" fill="var(--mantine-color-amber-7)">add above {add}%</text>
          </g>
        )}
        {remove != null && (
          <g>
            <line x1={pl} x2={W - pr} y1={y(remove)} y2={y(remove)} stroke="var(--mantine-color-teal-7)" strokeDasharray="5 4" vectorEffect="non-scaling-stroke" />
            <text x={W - pr} y={y(remove) - 4} fontSize="10" textAnchor="end" fill="var(--mantine-color-teal-7)">take away below {remove}%</text>
          </g>
        )}
        {joins?.filter((j) => new Date(j.at).getTime() >= start).map((j) => (
          <g key={j.at + j.label}>
            <line x1={x(j.at)} x2={x(j.at)} y1={pt} y2={pt + ih} stroke="var(--mantine-color-azure-6)" strokeDasharray="2 3" vectorEffect="non-scaling-stroke" />
            <circle cx={x(j.at)} cy={pt + 3} r="3" fill="var(--mantine-color-azure-6)" />
            <text x={x(j.at) + 5} y={pt + 12} fontSize="10" fill="var(--mantine-color-azure-6)">{j.label}</text>
          </g>
        ))}
        {empty && <text x={W / 2} y={H / 2} textAnchor="middle" fontSize="12" fill="var(--mantine-color-dimmed)">No metrics from the group's agents yet</text>}
      </svg>
    </Box>
  )
}

function averageOf(series: Record<string, ScalePoint[]>): ScalePoint[] {
  const sum = new Map<string, { s: number; n: number }>()
  for (const pts of Object.values(series)) {
    for (const p of pts) {
      const e = sum.get(p.at) ?? { s: 0, n: 0 }
      e.s += p.value
      e.n++
      sum.set(p.at, e)
    }
  }
  return [...sum.entries()].sort(([a], [b]) => a.localeCompare(b)).map(([at, e]) => ({ at, value: e.s / e.n }))
}

/** An add or take-away Argus asked for: the chart that met the rule, and what it costs (mockup 12). */
export function ScalePlanBlock({ plan, requestedAt, action, state }: { plan: ScalePlan; requestedAt: string; action: string; state: string }) {
  const lapses = new Date(new Date(requestedAt).getTime() + 30 * 60_000)
  const hours = plan.series?.length ? Math.max(1, Math.ceil((Date.now() - new Date(plan.series[0]!.at).getTime()) / 3600_000)) : 2
  return (
    <Stack gap={6} mt={8}>
      {plan.series && plan.series.length > 1 && (
        <Box maw={560}>
          <GroupChart average={plan.series} add={action === 'scale-add' ? plan.threshold : undefined}
            remove={action === 'scale-remove' ? plan.threshold : undefined} hours={Math.min(hours, 3)} height={140} />
        </Box>
      )}
      <Text size={FS.meta}>
        Group after: <b>{plan.after} of {plan.min}–{plan.max}</b>
        {plan.groupCost != null && <> · <b>{money(plan.groupCost)}</b>{plan.costCap ? <> of {money(plan.costCap)}</> : null} a month</>}
      </Text>
      {state === 'awaiting_approval' && (
        <Text size={FS.micro} c="dimmed">
          Lapses at {lapses.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })} if nobody answers. Argus asked, so anyone who
          approves changes here can approve it, and approving runs exactly this.
        </Text>
      )}
      {plan.automatic && plan.autoBy && <Text size={FS.micro} c="dimmed">Automatic, turned on by {plan.autoBy}.</Text>}
    </Stack>
  )
}
