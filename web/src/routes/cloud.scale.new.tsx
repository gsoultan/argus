import {
  Alert, Box, Button, Card, Group, MultiSelect, NumberInput, Radio, Select, SimpleGrid, Stack, Switch, Text, TextInput, Textarea, UnstyledButton,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconInfoCircle } from '@tabler/icons-react'
import { useEffect, useState, type ReactNode } from 'react'
import { EnvBadge } from '~/components/cloud/common'
import { RuleList } from '~/components/cloud/ScaleRules'
import { PageBody, PageHeader } from '~/components/page'
import { money, needsApproval, useAskCloudChange } from '~/lib/cloud'
import {
  DEFAULT_TRACK, limitsProblem, ruleProblem, ruleWords, scaleDraftQuery, trackProblem, trackWords, type AWSTrack, type ScaleLimits,
  type ScaleMode, type ScaleRules,
} from '~/lib/cloudScale'
import { guard } from '~/lib/guard'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS, SP } from '~/theme'

/**
 * Put a server in a scale group (mockup 06): what to copy, where traffic
 * comes in, how many, when to add and take away, and who decides. It starts
 * from a server that already does the job, with the answers worked out from
 * it, and ends on a plan like every change; the control plane checks all of
 * it again.
 */
export const Route = createFileRoute('/cloud/scale/new')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'admin'),
  validateSearch: (s: Record<string, unknown>): { account?: string; region?: string; from?: string } => ({
    account: typeof s.account === 'string' ? s.account : undefined,
    region: typeof s.region === 'string' ? s.region : undefined,
    from: typeof s.from === 'string' ? s.from : undefined,
  }),
  component: NewScaleGroup,
})

const STEPS = ['What to copy', 'Load balancer', 'Size', 'When and limits', 'Review'] as const
const NAME = /^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$/

function NewScaleGroup() {
  const navigate = useNavigate()
  const { account = '', region = '', from = '' } = Route.useSearch()
  const draft = useQuery(scaleDraftQuery(account, region, from))
  const d = draft.data
  const ask = useAskCloudChange()
  const [step, setStep] = useState(0)
  const [name, setName] = useState('')
  const [image, setImage] = useState('new')
  const [reboot, setReboot] = useState(false)
  const [targetGroup, setTargetGroup] = useState<string | null>(null)
  const [subnets, setSubnets] = useState<string[]>([])
  const [limits, setLimits] = useState<ScaleLimits>({ min: 2, max: 4, costCap: 0, addWait: 10, removeWait: 30 })
  const [rules, setRules] = useState<ScaleRules>({ add: [], remove: [] })
  const [mode, setMode] = useState<ScaleMode>('ask')
  const [track, setTrack] = useState<AWSTrack>(DEFAULT_TRACK)
  const [reason, setReason] = useState('')
  useEffect(() => {
    if (!d) return
    setName(d.name)
    setTargetGroup(d.targetGroup || d.targetGroups?.[0]?.value || null)
    const own = d.from.resource.links?.find((l) => l.kind === 'subnet')?.id
    setSubnets(own ? [own] : [])
    setLimits(d.limits)
    setRules(d.rules)
  }, [d])

  const src = d?.from.resource
  const tg = d?.targetGroups?.find((t) => t.value === targetGroup)
  const zones = new Set((d?.subnets ?? []).filter((s) => subnets.includes(s.value)).map((s) => s.zone))
  const rulesBad = mode === 'aws'
    ? trackProblem(track)
    : [...rules.add, ...rules.remove].map(ruleProblem).find(Boolean) || (rules.add.length ? '' : 'Give at least one rule for adding a server.')
  const problems = [
    !NAME.test(name) ? 'Name the group with lowercase letters, digits and dashes; its servers are named after it.' : '',
    !targetGroup ? 'Pick the target group traffic comes in through.' : '',
    subnets.length === 0 ? 'Pick at least one subnet.' : '',
    limitsProblem(limits, mode, d?.each),
    rulesBad,
  ]
  const stepProblem = [problems[0], problems[1], problems[2] || problems[3], problems[4] || problems[3], problems.find(Boolean) ?? ''][step] ?? ''
  const env = d?.environment ?? 'production'
  const waits = needsApproval(env, 'change')

  const submit = async () => {
    if (!d || !src) return
    try {
      const out = await ask.mutateAsync({
        accountId: account, region, action: 'scale-group-create', reason,
        spec: {
          name, from: src.id, image: image === 'new' ? '' : image, reboot, targetGroup, subnets, limits, rules, mode,
          ...(mode === 'aws' ? { track } : {}),
        },
      })
      if (out.state === 'failed') {
        notifyError('AWS refused it', new Error(out.error || 'No reason was given.'))
        return
      }
      if (out.state === 'awaiting_approval') {
        notifyOk('Asked for approval', `${out.summary}. Someone else approves it on Approvals.`)
        void navigate({ to: '/cloud', search: { tab: 'scaling' } })
        return
      }
      notifyOk('Scale group made', out.result.detail ?? out.summary)
      if (out.result.resourceId) void navigate({ to: '/cloud/scale/$groupId', params: { groupId: out.result.resourceId } })
      else void navigate({ to: '/cloud', search: { tab: 'scaling' } })
    } catch (err) {
      notifyError('Could not ask for the group', err)
    }
  }

  const header = (
    <PageHeader crumbs={[{ label: 'Cloud', to: '/cloud' }, { label: 'Scaling', to: '/cloud?tab=scaling' }, { label: 'New scale group' }]}
      title={src ? `Put ${src.name} in a scale group` : 'New scale group'} status={d ? <EnvBadge env={env} /> : undefined} />
  )
  if (!account || !from) {
    return (
      <>
        {header}
        <PageBody>
          <Alert color="sky" variant="light" maw={720}>
            <Text size={FS.meta}>A scale group starts from a server. Open a running instance under Cloud › Instances and choose Put in a scale group, or New scale group on Cloud › Scaling.</Text>
          </Alert>
        </PageBody>
      </>
    )
  }
  if (draft.isError) {
    return (
      <>
        {header}
        <PageBody><Alert color="rose" variant="light" maw={720}><Text size={FS.meta}>{(draft.error as Error).message}</Text></Alert></PageBody>
      </>
    )
  }

  const each = d?.each ?? null
  return (
    <>
      {header}
      <PageBody>
        <Stack gap="md" maw={1180}>
          <StepBar step={step} onStep={(i) => i < step && setStep(i)} />
          {d?.problem && <Alert color="amber" variant="light"><Text size={FS.meta}>{d.problem}</Text></Alert>}
          <SimpleGrid cols={{ base: 1, md: 3 }} spacing="md">
            <Box style={{ gridColumn: 'span 2' }}>
              <Card padding="lg">
                {step === 0 && src && (
                  <Stack gap="sm">
                    <TextInput label="Group name" description="Its new servers are named after it: web-02, web-03…" value={name}
                      onChange={(e) => setName(e.currentTarget.value.toLowerCase())} error={name && !NAME.test(name) ? problems[0] : undefined} maw={320} />
                    <Radio.Group label={`What a new server is copied from`} value={image} onChange={setImage}>
                      <Stack gap={SP.cozy} mt={SP.snug}>
                        <Radio value="new" label={`A new image of ${src.name}, made now`}
                          description={`${src.size}, ${d?.diskGb ?? 20} GiB disk, its security groups and tags, and the Argus agent on first boot.`} />
                        {(d?.images ?? []).map((im) => <Radio key={im.value} value={im.value} label={`Image ${im.label}`} description={`Made of ${src.name} before (${im.value})`} />)}
                      </Stack>
                    </Radio.Group>
                    {image === 'new' && (
                      <Switch checked={reboot} onChange={(e) => setReboot(e.currentTarget.checked)} label="Reboot it for a consistent disk"
                        description="Without a reboot the image is taken while it runs, which is fine for most servers; a database writing at that moment is not." />
                    )}
                  </Stack>
                )}
                {step === 1 && (
                  <Stack gap="sm">
                    <Select label="Where traffic comes in" description="The target group its servers join, behind its load balancer" data={(d?.targetGroups ?? []).map((t) => ({
                      value: t.value, label: t.note ? `${t.label} · behind ${t.note}` : `${t.label} · no load balancer in front yet`,
                    }))} value={targetGroup} onChange={setTargetGroup} allowDeselect={false} />
                    {d?.targetGroup && targetGroup === d.targetGroup && <Text size={FS.meta} c="dimmed">{src?.name} is already in it.</Text>}
                    {targetGroup && targetGroup !== d?.targetGroup && <Text size={FS.meta} c="dimmed">{src?.name} is put in it when the group is made.</Text>}
                    <Text size={FS.micro} c="dimmed">No target group yet? Make a load balancer and target group under Cloud › Network first.</Text>
                  </Stack>
                )}
                {step === 2 && (
                  <Stack gap="sm">
                    <Group grow align="flex-start">
                      <NumberInput label="Fewest servers" min={1} max={20} value={limits.min} onChange={(v) => setLimits({ ...limits, min: Number(v) || 0 })} />
                      <NumberInput label="Most servers" description="Required; Argus never goes above it" min={1} max={20} value={limits.max}
                        onChange={(v) => setLimits({ ...limits, max: Number(v) || 0 })} />
                    </Group>
                    <MultiSelect label="Subnets" description="One in each zone spreads the servers, so a zone going down takes only some"
                      data={(d?.subnets ?? []).map((s) => ({ value: s.value, label: `${s.label || s.value} · ${s.zone}` }))} value={subnets} onChange={setSubnets} />
                    {problems[3] && step === 2 && <Text size={FS.meta} c="rose">{problems[3]}</Text>}
                  </Stack>
                )}
                {step === 3 && (
                  <Stack gap="md">
                    {mode === 'aws' ? (
                      <Box>
                        <Text fw={600} size={FS.body} mb={SP.cozy}>AWS keeps</Text>
                        <Group align="flex-end" gap="sm">
                          <Select label="Measure" w={260} allowDeselect={false} value={track.metric}
                            data={[{ value: 'cpu', label: "The group's average processor" }, { value: 'requests', label: 'Requests per server a minute' }]}
                            onChange={(v) => setTrack(v === 'requests' ? { metric: 'requests', target: 1000 } : { metric: 'cpu', target: 60 })} />
                          <NumberInput label="Near" w={140} suffix={track.metric === 'cpu' ? ' %' : ''} min={1} value={track.target}
                            onChange={(v) => setTrack({ ...track, target: Number(v) || 0 })} />
                        </Group>
                        <Text size={FS.micro} c="dimmed" mt={SP.snug}>
                          On CloudWatch's free 5-minute metrics, so AWS does not see memory, swap or load. Argus's own rules wait until Argus decides again.
                          {track.metric === 'requests' ? ' Requests need an application load balancer in front of the target group.' : ''}
                        </Text>
                      </Box>
                    ) : (
                      <>
                        <RuleList title="Add a server when" join="or" rules={rules.add} onChange={(add) => setRules({ ...rules, add })} fresh={{ signal: 'cpu', over: 'average', op: 'above', value: 75, minutes: 10 }} />
                        <RuleList title="Take one away when" join="and" rules={rules.remove} onChange={(remove) => setRules({ ...rules, remove })} fresh={{ signal: 'cpu', over: 'average', op: 'below', value: 25, minutes: 30 }} />
                      </>
                    )}
                    <SimpleGrid cols={{ base: 1, sm: 3 }}>
                      <NumberInput label="Cost cap" description="A month; never more, whatever the metrics say" prefix="$" min={0} value={limits.costCap}
                        onChange={(v) => setLimits({ ...limits, costCap: Number(v) || 0 })} />
                      <NumberInput label="Wait after adding" suffix=" min" min={5} max={240} value={limits.addWait} onChange={(v) => setLimits({ ...limits, addWait: Number(v) || 0 })} />
                      <NumberInput label="Wait after taking away" suffix=" min" min={5} max={240} value={limits.removeWait} onChange={(v) => setLimits({ ...limits, removeWait: Number(v) || 0 })} />
                    </SimpleGrid>
                    <Box>
                      <Text fw={600} size={FS.body} mb={SP.cozy}>Who decides</Text>
                      <SimpleGrid cols={{ base: 1, sm: 3 }} spacing="sm">
                        <ModeCard on={mode === 'ask'} onPick={() => setMode('ask')} title="Ask each time" tag="Recommended"
                          text="Each add or take-away goes to Approvals with the chart. Unanswered in 30 minutes, it lapses." />
                        <ModeCard on={mode === 'auto'} onPick={() => setMode('auto')} title="Automatic within limits"
                          text={`Argus acts between ${limits.min} and ${limits.max} servers${limits.costCap ? ` and under ${money(limits.costCap)} a month` : ''}, at most 3 adds a day before it asks.${waits ? ' Turning this on here needs an approval.' : ''}`} />
                        <ModeCard on={mode === 'aws'} onPick={() => setMode('aws')} title="AWS decides"
                          text={`AWS Auto Scaling on CloudWatch, keeping ${trackWords(track)}. Keeps working if Argus is down; sees no memory.`} />
                      </SimpleGrid>
                    </Box>
                    {rulesBad && <Text size={FS.meta} c="rose">{rulesBad}</Text>}
                    {mode === 'aws' && problems[3] && <Text size={FS.meta} c="rose">{problems[3]}</Text>}
                  </Stack>
                )}
                {step === 4 && src && (
                  <Stack gap="sm">
                    <Text fw={600} size={FS.body}>Argus will</Text>
                    <Stack gap={4} component="ol" style={{ paddingLeft: 18, margin: 0 }}>
                      <Text component="li" size={FS.body}>{image === 'new' ? `Make an image of ${src.name}${reboot ? ', rebooting it' : ', without a reboot'}` : `Launch from image ${image}`}.</Text>
                      <Text component="li" size={FS.body}>Keep {limits.min} to {limits.max} servers like it ({d?.size}) in {tg?.label}{tg?.note ? `, behind ${tg.note}` : ''}, across {zones.size} zone{zones.size === 1 ? '' : 's'}.</Text>
                      <Text component="li" size={FS.body}>Make an AWS Auto Scaling group, {floorName(name)}, once the image is ready, that {mode === 'aws'
                        ? `adds and takes away copies between ${Math.max(limits.min - 1, 0)} and ${Math.max(limits.max - 1, 1)}, keeping ${trackWords(track)}`
                        : `keeps ${copies(limits.min - 1)} besides ${src.name} even when Argus is down`}. Its servers enroll their agent with the group's key.</Text>
                      {mode !== 'aws' && <Text component="li" size={FS.body}>Add one when {rules.add.map(ruleWords).join(', or ')}.</Text>}
                      {mode !== 'aws' && <Text component="li" size={FS.body}>{rules.remove.length ? `Take the newest it launched away, drained first, when ${rules.remove.map(ruleWords).join(', and ')}.` : 'Never take one away by itself.'}</Text>}
                      <Text component="li" size={FS.body}>{mode === 'ask' ? 'Ask each time, in Approvals.' : mode === 'auto' ? 'Act by itself within the limits, naming you as who turned it on.' : 'Record what AWS did on the group page.'}</Text>
                    </Stack>
                    <Textarea label="Reason" placeholder="Why this group, for whoever approves it and the audit log" autosize minRows={2} value={reason}
                      onChange={(e) => setReason(e.currentTarget.value)} maxLength={500} />
                    {waits && <Text size={FS.meta} c="dimmed">This is {env}, so someone else approves it before anything is made.</Text>}
                  </Stack>
                )}
              </Card>
            </Box>
            <Stack gap="md">
              <Card padding="md">
                <Text fw={600} size={FS.body} mb="xs">So far</Text>
                <Stack gap={6}>
                  <Fact label="Copy of">{src ? `${src.name} · ${image === 'new' ? `new image${reboot ? '' : ' (no reboot)'}` : image}` : '…'}</Fact>
                  <Fact label="Traffic">{tg ? `${tg.label}${tg.note ? ` · behind ${tg.note}` : ''}` : '—'}</Fact>
                  <Fact label="Size">{limits.min} to {limits.max}{zones.size ? ` · ${[...zones].map((z) => z?.slice(-2)).join(', ')}` : ''}</Fact>
                  <Fact label="Members">{src?.name ?? '—'}</Fact>
                </Stack>
              </Card>
              {each != null && (
                <Card padding="md">
                  <Text size={FS.micro} c="dimmed">Cost</Text>
                  <Text fw={700} size="lg">{money(each * limits.min)} → {money(each * limits.max)} a month</Text>
                  <Text size={FS.micro} c="dimmed">{limits.min} server{limits.min === 1 ? '' : 's'} to {limits.max} at {money(each)} each{tg?.note ? `, plus ${tg.note} (exists)` : ''}</Text>
                </Card>
              )}
              <Alert color="sky" variant="light" icon={<IconInfoCircle size={16} />} title="If Argus is down">
                <Text size={FS.meta}>{mode === 'aws'
                  ? 'Nothing changes: AWS Auto Scaling keeps adding and taking away servers by itself.'
                  : `The group never falls below ${limits.min}: AWS Auto Scaling replaces a failed server by itself. Growing above ${limits.min} waits for Argus.`}</Text>
              </Alert>
              <Group justify="space-between">
                <Button variant="default" disabled={step === 0} onClick={() => setStep(step - 1)}>Back</Button>
                {step < STEPS.length - 1
                  ? <Button disabled={!!stepProblem || !d || !!d.problem} onClick={() => setStep(step + 1)}>Next: {STEPS[step + 1]?.toLowerCase()}</Button>
                  : <Button disabled={!!stepProblem || !!d?.problem} loading={ask.isPending} onClick={() => void submit()}>{waits ? 'Ask for approval' : 'Make the group'}</Button>}
              </Group>
              {stepProblem && step < 4 && <Text size={FS.micro} c="dimmed">{stepProblem}</Text>}
            </Stack>
          </SimpleGrid>
        </Stack>
      </PageBody>
    </>
  )
}

function floorName(group: string): string {
  return `argus-${group || 'group'}`
}

function copies(n: number): string {
  if (n <= 0) return 'no copies'
  return `${n} cop${n === 1 ? 'y' : 'ies'}`
}

function StepBar({ step, onStep }: { step: number; onStep: (i: number) => void }) {
  return (
    <SimpleGrid cols={{ base: 2, sm: 5 }} spacing="sm">
      {STEPS.map((s, i) => {
        const color = i < step ? 'var(--mantine-color-teal-6)' : i === step ? 'var(--mantine-color-azure-6)' : 'var(--color-line)'
        return (
          <UnstyledButton key={s} onClick={() => onStep(i)} aria-current={i === step ? 'step' : undefined}>
            <Box h={3} style={{ background: color, borderRadius: 2 }} />
            <Text size={FS.meta} mt={6} c={i < step ? 'teal' : i === step ? 'azure' : 'dimmed'}>{i + 1} · {s}</Text>
          </UnstyledButton>
        )
      })}
    </SimpleGrid>
  )
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <Group gap="sm" wrap="nowrap" align="flex-start">
      <Text size={FS.meta} c="dimmed" w={70} style={{ flexShrink: 0 }}>{label}</Text>
      <Text size={FS.meta}>{children}</Text>
    </Group>
  )
}

function ModeCard({ on, onPick, title, text, tag }: { on: boolean; onPick: () => void; title: string; text: string; tag?: string }) {
  return (
    <UnstyledButton onClick={onPick} aria-pressed={on} p="sm"
      style={{ border: `${on ? 2 : 1}px solid ${on ? 'var(--mantine-color-azure-6)' : 'var(--color-line)'}`, borderRadius: 8 }}>
      <Text fw={600} size={FS.body}>{title}</Text>
      <Text size={FS.meta} c="dimmed" mt={4}>{text}</Text>
      {tag && <Text size={FS.micro} c="azure" mt={6} fw={600}>{tag}</Text>}
    </UnstyledButton>
  )
}
