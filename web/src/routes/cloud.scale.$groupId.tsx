import { Alert, Badge, Box, Button, Card, Group, Modal, NumberInput, SegmentedControl, SimpleGrid, Stack, Table, Text, Textarea, Tooltip } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute } from '@tanstack/react-router'
import { IconInfoCircle, IconPlayerPause, IconPlayerPlay, IconPlus } from '@tabler/icons-react'
import { useState } from 'react'
import { EnvBadge } from '~/components/cloud/common'
import { GroupChart } from '~/components/cloud/ScaleGroups'
import { RuleList } from '~/components/cloud/ScaleRules'
import { DataTable, PageBody, PageHeader, SectionCard } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import { money, needsApproval, useAskCloudChange, type CloudChangeInput } from '~/lib/cloud'
import {
  MODE_WORDS, askerName, limitsProblem, ruleProblem, ruleWords, scaleGroupQuery, serving, type ScaleGroupDetail, type ScaleLimits, type ScaleMode, type ScaleRules,
} from '~/lib/cloudScale'
import { guard } from '~/lib/guard'
import { notifyError, notifyOk } from '~/lib/notify'
import { meQuery } from '~/lib/queries'
import { canSee } from '~/lib/roles'
import { FS } from '~/theme'

/**
 * One scale group (mockup 11): how busy it is against its rules, its
 * servers, and what it did and why. Pause, change the rules, or add a
 * server now, each a change like any other.
 */
export const Route = createFileRoute('/cloud/scale/$groupId')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  component: ScaleGroupPage,
})

const MEMBER_STATE: Record<string, { label: string; color: string }> = {
  joining: { label: 'starting', color: 'sky' }, member: { label: 'serving', color: 'teal' }, draining: { label: 'draining', color: 'amber' },
  gone: { label: 'gone', color: 'slate' }, failed: { label: 'failed', color: 'rose' },
}

const ACTIVITY_DOT: Record<string, string> = {
  added: 'azure', removed: 'azure', terminated: 'azure', healthy: 'teal', joined: 'teal', rule: 'orange', minimum: 'orange', asked: 'amber',
  lapsed: 'slate', declined: 'slate', failed: 'rose', paused: 'rose', created: 'slate', changed: 'slate', resumed: 'slate', gone: 'slate',
}

function ScaleGroupPage() {
  const { groupId } = Route.useParams()
  const { data: g, isPending, error } = useQuery(scaleGroupQuery(groupId))
  const { data: me } = useQuery(meQuery())
  const admin = canSee(me?.role, 'admin')
  const ask = useAskCloudChange()
  const [editing, setEditing] = useState(false)
  if (isPending) return <><PageHeader crumbs={[{ label: 'Cloud', to: '/cloud' }, { label: 'Scaling', to: '/cloud?tab=scaling' }]} title="Scale group" /><PageBody><Text c="dimmed" size={FS.meta}>Loading…</Text></PageBody></>
  if (!g) return <><PageHeader crumbs={[{ label: 'Cloud', to: '/cloud' }, { label: 'Scaling', to: '/cloud?tab=scaling' }]} title="Scale group" /><PageBody><Alert color="rose" variant="light">{(error as Error | null)?.message ?? 'No such scale group.'}</Alert></PageBody></>

  const n = serving(g).length
  const healthy = g.members.filter((m) => m.state === 'member' && g.health[m.instanceId] === 'healthy').length
  const add = g.rules.add.find((r) => r.signal === 'cpu')
  const remove = g.rules.remove.find((r) => r.signal === 'cpu')
  const joins = g.members.filter((m) => m.instanceId !== g.template.from).map((m) => ({ at: m.joinedAt, label: `${m.hostname} joined` }))
  const change = async (input: Omit<CloudChangeInput, 'accountId' | 'region' | 'resourceId'>, done: string) => {
    try {
      const out = await ask.mutateAsync({ accountId: g.accountId, region: g.region, resourceId: g.id, ...input })
      if (out.state === 'awaiting_approval') notifyOk('Asked for approval', `${out.summary}. Someone else approves it on Approvals.`)
      else if (out.state === 'failed') notifyError('It did not run', new Error(out.error || 'No reason was given.'))
      else notifyOk(done, out.summary)
      return true
    } catch (err) {
      notifyError('Could not ask for it', err)
      return false
    }
  }
  const atMax = n >= g.limits.max
  return (
    <>
      <PageHeader crumbs={[{ label: 'Cloud', to: '/cloud' }, { label: 'Scaling', to: '/cloud?tab=scaling' }]} title={g.name}
        status={
          <Group gap={6}>
            <Badge variant="light" color={healthy === n ? 'teal' : 'amber'} radius="sm">{healthy} healthy</Badge>
            <Badge variant="light" color={g.state === 'paused' ? 'slate' : 'azure'} radius="sm">{g.state === 'paused' ? 'Paused' : MODE_WORDS[g.mode]}</Badge>
            <EnvBadge env={g.environment} />
          </Group>
        }
        description={`${g.accountName} · ${g.region} · behind ${g.balancer || g.targetName} · ${g.limits.min} to ${g.limits.max} servers${g.limits.costCap ? ` · cap ${money(g.limits.costCap)} a month` : ''}${g.monthly != null ? ` (now ${money(g.monthly)})` : ''} · ${g.template.size} from ${g.template.imageLabel || g.template.image}`}
        actions={admin ? (
          <Group gap="xs">
            {g.state === 'active'
              ? <Button variant="default" leftSection={<IconPlayerPause size={15} />} loading={ask.isPending}
                  onClick={() => void change({ action: 'scale-group-change', spec: { pause: true } }, 'Paused')}>Pause</Button>
              : <Button variant="default" leftSection={<IconPlayerPlay size={15} />} loading={ask.isPending}
                  onClick={() => void change({ action: 'scale-group-change', spec: { pause: false } }, 'Resumed')}>Resume</Button>}
            <Button variant="default" onClick={() => setEditing(true)}>Change rules</Button>
            <Tooltip label={atMax ? `At its maximum of ${g.limits.max}` : 'Launch one more now, through the usual approval'} disabled={false}>
              <Button variant="default" leftSection={<IconPlus size={15} />} disabled={atMax || !!g.open} loading={ask.isPending}
                onClick={() => void change({ action: 'scale-add', reason: 'Added by hand from the group page' }, 'Adding a server')}>Add a server now</Button>
            </Tooltip>
          </Group>
        ) : undefined} />
      <PageBody>
        <Stack gap="md">
          {g.state === 'paused' && g.pausedReason && <Alert color="slate" variant="light" title="Paused"><Text size={FS.meta}>{g.pausedReason}</Text></Alert>}
          {g.open && (
            <Alert color="amber" variant="light" title={g.open.state === 'awaiting_approval' ? `Asked ${relTime(g.open.requestedAt)} by ${askerName(g.open.requestedBy)}, waiting` : 'Running'}>
              <Text size={FS.meta}>{g.open.summary}. {g.open.state === 'awaiting_approval' ? 'Approve or decline it on Approvals.' : ''}</Text>
            </Alert>
          )}
          {g.note && !g.open && <Text size={FS.meta} c="dimmed" data-testid="scale-note">{g.note}{g.checkedAt ? ` · looked ${relTime(g.checkedAt)}` : ''}</Text>}
          <SimpleGrid cols={{ base: 1, lg: 3 }} spacing="md">
            <Box style={{ gridColumn: 'span 2' }}>
              <Card padding={0}>
                <Box p="md" pb={0}>
                  <Group gap="xs"><Text fw={600} size={FS.body}>Processor across the group</Text><Text size={FS.micro} c="dimmed">last 8 h</Text></Group>
                  <GroupChart series={g.series} add={add?.value} remove={remove?.value} joins={joins} />
                </Box>
                <DataTable minWidth={620} columns={['Member', 'Zone', 'Processor', 'Memory', 'Target', 'Since']}>
                  {g.members.filter((m) => m.state !== 'gone').map((m) => {
                    const u = g.usage[m.hostname]
                    const st = MEMBER_STATE[m.state]!
                    const h = g.health[m.instanceId]
                    return (
                      <Table.Tr key={m.instanceId}>
                        <Table.Td><Text fw={600} size={FS.meta}>{m.hostname}</Text><Text size={FS.micro} c="dimmed" ff="monospace">{m.instanceId}</Text></Table.Td>
                        <Table.Td><Text size={FS.meta}>{m.zone ? m.zone.slice(-2) : '—'}</Text></Table.Td>
                        <Table.Td><Text size={FS.meta}>{u?.cpu != null ? `${Math.round(u.cpu)}%` : '—'}</Text></Table.Td>
                        <Table.Td><Text size={FS.meta}>{u?.mem != null ? `${Math.round(u.mem)}%` : '—'}</Text></Table.Td>
                        <Table.Td>
                          {m.state === 'member' || m.state === 'joining'
                            ? <Badge variant="light" radius="sm" color={h === 'healthy' ? 'teal' : h ? 'amber' : 'slate'}>{h || (m.registeredAt ? 'checking' : 'not yet in')}</Badge>
                            : <Badge variant="light" radius="sm" color={st.color}>{st.label}</Badge>}
                        </Table.Td>
                        <Table.Td><Tooltip label={absTime(m.joinedAt)}><Text size={FS.meta} c="dimmed">{relTime(m.joinedAt)}</Text></Tooltip></Table.Td>
                      </Table.Tr>
                    )
                  })}
                </DataTable>
              </Card>
            </Box>
            <SectionCard title="Activity">
              <Stack gap={0}>
                {g.activity.length === 0 && <Text size={FS.meta} c="dimmed">Nothing yet.</Text>}
                {g.activity.map((a, i) => (
                  <Group key={i} gap="sm" wrap="nowrap" align="flex-start" py={8} style={{ borderTop: i ? '1px solid var(--color-line)' : undefined }}>
                    <Tooltip label={absTime(a.at)}><Text size={FS.micro} c="dimmed" ff="monospace" w={74} style={{ flexShrink: 0 }}>{relTime(a.at)}</Text></Tooltip>
                    <Box mt={5} w={7} h={7} style={{ borderRadius: 7, flexShrink: 0, background: `var(--mantine-color-${ACTIVITY_DOT[a.kind] ?? 'slate'}-6)` }} />
                    <Text size={FS.meta}>{a.detail}{a.actor !== 'argus' && a.actor !== 'argus-scale' && !a.detail.includes(a.actor) ? ` · ${a.actor}` : ''}</Text>
                  </Group>
                ))}
                <Text size={FS.micro} c="dimmed" mt="xs">Argus's own adds and take-aways run as argus-scale in CloudTrail; automatic ones name the person who turned automatic on.</Text>
              </Stack>
            </SectionCard>
          </SimpleGrid>
          <SectionCard title="Rules" icon={IconInfoCircle}>
            <Stack gap={4}>
              <Text size={FS.meta}><b>Add a server</b> when {g.rules.add.map(ruleWords).join(', or ')}.</Text>
              <Text size={FS.meta}><b>Take one away</b> {g.rules.remove.length ? `when ${g.rules.remove.map(ruleWords).join(', and ')}` : 'never by itself'}.</Text>
              <Text size={FS.meta} c="dimmed">
                Waits {g.limits.addWait} min after adding and {g.limits.removeWait} after taking away. At most 3 automatic adds a day; two new servers in a row that do not become healthy pause the group.
                {g.mode === 'auto' && g.autoBy ? ` Automatic since ${absTime(g.autoAt ?? g.createdAt)}, turned on by ${g.autoBy}.` : ''}
              </Text>
            </Stack>
          </SectionCard>
        </Stack>
      </PageBody>
      {editing && <EditRules g={g} busy={ask.isPending} onClose={() => setEditing(false)}
        onSave={async (spec, reason) => { if (await change({ action: 'scale-group-change', spec, reason }, 'Changed')) setEditing(false) }} />}
    </>
  )
}

function EditRules({ g, busy, onClose, onSave }: {
  g: ScaleGroupDetail; busy: boolean; onClose: () => void
  onSave: (spec: { limits: ScaleLimits; rules: ScaleRules; mode: ScaleMode }, reason: string) => void
}) {
  const [limits, setLimits] = useState(g.limits)
  const [rules, setRules] = useState(g.rules)
  const [mode, setMode] = useState<ScaleMode>(g.mode)
  const [reason, setReason] = useState('')
  const problem = limitsProblem(limits, mode) || [...rules.add, ...rules.remove].map(ruleProblem).find(Boolean) || (rules.add.length ? '' : 'Give at least one rule for adding a server.')
  const waits = needsApproval(g.environment, 'change')
  return (
    <Modal opened onClose={onClose} title={`Change ${g.name}`} size="xl">
      <Stack gap="md">
        <RuleList title="Add a server when" join="or" rules={rules.add} onChange={(add) => setRules({ ...rules, add })} fresh={{ signal: 'cpu', over: 'average', op: 'above', value: 75, minutes: 10 }} />
        <RuleList title="Take one away when" join="and" rules={rules.remove} onChange={(remove) => setRules({ ...rules, remove })} fresh={{ signal: 'cpu', over: 'average', op: 'below', value: 25, minutes: 30 }} />
        <SimpleGrid cols={{ base: 2, sm: 5 }}>
          <NumberInput label="Fewest" min={1} max={20} value={limits.min} onChange={(v) => setLimits({ ...limits, min: Number(v) || 0 })} />
          <NumberInput label="Most" min={1} max={20} value={limits.max} onChange={(v) => setLimits({ ...limits, max: Number(v) || 0 })} />
          <NumberInput label="Cost cap" prefix="$" min={0} value={limits.costCap} onChange={(v) => setLimits({ ...limits, costCap: Number(v) || 0 })} />
          <NumberInput label="Wait after adding" suffix=" min" value={limits.addWait} onChange={(v) => setLimits({ ...limits, addWait: Number(v) || 0 })} />
          <NumberInput label="After taking away" suffix=" min" value={limits.removeWait} onChange={(v) => setLimits({ ...limits, removeWait: Number(v) || 0 })} />
        </SimpleGrid>
        <Box>
          <Text size={FS.meta} fw={600} mb={6}>Who decides</Text>
          <SegmentedControl value={mode} onChange={(v) => setMode(v as ScaleMode)} data={[{ value: 'ask', label: 'Ask each time' }, { value: 'auto', label: 'Automatic within limits' }]} />
        </Box>
        <Textarea label="Reason" autosize minRows={2} value={reason} onChange={(e) => setReason(e.currentTarget.value)} maxLength={500} />
        {problem && <Text size={FS.meta} c="rose">{problem}</Text>}
        <Group justify="space-between">
          <Text size={FS.micro} c="dimmed">{waits ? `This is ${g.environment}, so someone else approves the change.` : 'It takes effect at once.'}</Text>
          <Group gap="xs">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button disabled={!!problem} loading={busy} onClick={() => onSave({ limits, rules, mode }, reason)}>{waits ? 'Ask for approval' : 'Save'}</Button>
          </Group>
        </Group>
      </Stack>
    </Modal>
  )
}
