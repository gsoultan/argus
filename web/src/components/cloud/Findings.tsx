import {
  Alert, Badge, Box, Button, Checkbox, Code, Drawer, Group, Modal, Radio, SegmentedControl, Select, SimpleGrid, Stack, Table, Text,
  Textarea, TextInput, Tooltip,
} from '@mantine/core'
import { useMediaQuery } from '@mantine/hooks'
import { useQueries, useQuery } from '@tanstack/react-query'
import { IconAlertTriangle, IconBookmark, IconCircleCheck, IconCoin, IconLockOpen, IconShield, IconTrash } from '@tabler/icons-react'
import { useMemo, useState } from 'react'
import { EmptyState } from '~/components/page'
import { Field, Stat, relTime } from '~/components/primitives'
import { cloudChangesQuery, cloudItemsQuery, envOf, needsApproval, useAskCloudChange, type CloudChange, type CloudItem, type CloudKind } from '~/lib/cloud'
import { dollars } from '~/lib/cloudCosts'
import {
  GROUP_LABEL, SEVERITY_COLOR, SEVERITY_LABEL, SOURCE_HELP, SOURCE_LABEL, cloudFindingsQuery, findingCounts, keepAllowed, useKeepFinding,
  useUnkeepFinding, ruleChangeFor, type CloudFinding, type FindingGroup, type FindingSeverity, type KeepInput,
} from '~/lib/cloudFindings'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS, INK, SP } from '~/theme'
import { EnvBadge, ResourceDrawer } from './common'
import { SettingsChange } from './InstanceActions'
import { CancelCapacity } from './CapacityActions'
import { cloudReservationsQuery, type ReservationItem } from '~/lib/cloudReservations'
import { RemoveRuleChange, ruleText } from './NetworkActions'

/*
 * Cloud › Risks & waste. Everything here is a suggestion: nothing changes in
 * AWS from this tab. A fix opens the change Argus already has, filled in,
 * with its own preview, approval and audit entry. "Keep it" hides a finding
 * on purpose, with a reason everyone with Cloud access can see.
 */

const INVENTORY: CloudKind[] = ['instance', 'volume', 'firewall', 'address', 'snapshot', 'image', 'load-balancer']

const FIX_LABEL: Record<string, string> = {
  'firewall-rules': 'Remove the rule', 'firewall-delete': 'Delete group', 'volume-delete': 'Delete disk', 'address-release': 'Release',
  'snapshot-delete': 'Delete', 'image-delete': 'Delete image', stop: 'Stop', settings: 'Require tokens',
  'capacity-cancel': 'Cancel it',
}

type Filter = 'all' | FindingGroup

export function FindingsPanel({ names, admin }: { names: Map<string, string>; admin: boolean }) {
  const q = useQuery(cloudFindingsQuery())
  const changes = useQuery(cloudChangesQuery())
  const inv = useQueries({ queries: INVENTORY.map((k) => cloudItemsQuery(k)) })
  const phone = useMediaQuery('(max-width: 48em)')
  const [filter, setFilter] = useState<Filter>('all')
  const [account, setAccount] = useState<string | null>(null)
  const [severity, setSeverity] = useState<string | null>(null)
  const [showKept, setShowKept] = useState(false)
  const [open, setOpen] = useState<CloudFinding | null>(null)
  const [keeping, setKeeping] = useState<CloudFinding | null>(null)
  const [resource, setResource] = useState<CloudItem | null>(null)
  const [rule, setRule] = useState<{ it: CloudItem; f: CloudFinding } | null>(null)
  const [settings, setSettings] = useState<CloudItem | null>(null)
  const [capacity, setCapacity] = useState<ReservationItem | null>(null)
  const reservations = useQuery({ ...cloudReservationsQuery(), enabled: cloudReservationsQuery().enabled && admin })
  const [picked, setPicked] = useState<Set<string>>(new Set())
  const [bulk, setBulk] = useState(false)

  const items = useMemo(() => {
    const m = new Map<string, CloudItem>()
    inv.forEach((r) => r.data?.forEach((it) => m.set(it.accountId + '/' + it.resource.id, it)))
    return m
  }, [inv])
  const itemOf = (f: CloudFinding) => items.get(f.accountId + '/' + f.resourceId)
  const askedOf = (f: CloudFinding) => ruleChangeFor(f, changes.data, itemOf(f)?.seenAt)
  const capacityOf = (f: CloudFinding) => f.fix === 'capacity-cancel'
    ? reservations.data?.capacityReservations.find((c) => c.accountId === f.accountId && c.resource.id === f.resourceId)
    : undefined

  const all = q.data?.items ?? []
  const counts = findingCounts(all)
  const shown = (showKept ? q.data?.kept ?? [] : all).filter((f) =>
    (filter === 'all' || f.group === filter) && (!account || f.accountId === account) && (!severity || f.severity === severity))
  const unread = (q.data?.accounts ?? []).filter((a) => !a.accountChecks)
  const pickedFindings = all.filter((f) => picked.has(f.id))

  const fix = (f: CloudFinding) => {
    const it = itemOf(f)
    const cr = capacityOf(f)
    if (cr) setCapacity(cr)
    else if (f.fix === 'firewall-rules' && f.rule && it) setRule({ it, f })
    else if (f.fix === 'settings' && it) setSettings(it)
    else if (it) setResource(it)
    else setOpen(f)
  }
  const fixLabel = (f: CloudFinding) => {
    if (capacityOf(f)) return FIX_LABEL['capacity-cancel']!
    if (!itemOf(f)) return f.howToFix ? 'How to fix' : 'Details'
    return (f.fix && FIX_LABEL[f.fix]) || 'Open'
  }
  const toggle = (f: CloudFinding) => {
    const next = new Set(picked)
    if (next.has(f.id)) next.delete(f.id)
    else next.add(f.id)
    setPicked(next)
  }

  if (q.error) return <Alert color="rose" variant="light">{(q.error as Error).message}</Alert>

  return (
    <Stack gap="lg">
      <SimpleGrid cols={{ base: 2, md: 4 }} spacing="md">
        <Stat label="High risks" value={q.data ? counts.high : undefined} icon={IconLockOpen} tone={counts.high ? 'warn' : 'ok'} color="rose"
          sub="Open to the internet, shared with everyone, or root without MFA" onClick={() => { setFilter('risk'); setSeverity('high') }} />
        <Stat label="Other risks" value={q.data ? counts.otherRisks : undefined} icon={IconShield} tone={counts.otherRisks ? 'warn' : 'ok'} color="amber"
          sub="Worth fixing this month" onClick={() => { setFilter('risk'); setSeverity(null) }} />
        <Stat label="Paid for, not in use" value={q.data ? dollars(q.data.monthly) : undefined} icon={IconCoin} tone={q.data?.monthly ? 'warn' : 'ok'} color="amber"
          sub={q.data ? `a month, across ${counts.waste} resource${counts.waste === 1 ? '' : 's'}` : undefined} onClick={() => setFilter('waste')} />
        <Stat label="Firewall rules to tidy" value={q.data ? counts.rules : undefined} icon={IconShield}
          sub="Groups used by nothing, rules nothing needs" onClick={() => setFilter('rules')} />
      </SimpleGrid>

      <Group gap="sm" wrap="wrap">
        <SegmentedControl size="xs" value={filter} onChange={(v) => setFilter(v as Filter)} data={[
          { value: 'all', label: `Everything · ${all.length}` },
          { value: 'risk', label: `Security risks · ${counts.risks}` },
          { value: 'waste', label: `Not in use · ${counts.waste}` },
          { value: 'rules', label: `Firewall rules · ${counts.rules}` },
        ]} />
        <Select size="xs" w={200} placeholder="All accounts" clearable value={account} onChange={setAccount}
          data={(q.data?.accounts ?? []).map((a) => ({ value: a.id, label: a.name }))} aria-label="Account" />
        <Select size="xs" w={150} placeholder="Any severity" clearable value={severity} onChange={setSeverity}
          data={(['high', 'medium', 'low'] as FindingSeverity[]).map((s) => ({ value: s, label: SEVERITY_LABEL[s] }))} aria-label="Severity" />
        <Group gap={6} ml="auto">
          <Text size={FS.meta} c="dimmed">{q.data ? `Checked with the last read, ${relTime(q.data.checkedAt)}` : ''}</Text>
          <Button size="compact-xs" variant={showKept ? 'light' : 'subtle'} leftSection={<IconBookmark size={13} />} onClick={() => setShowKept(!showKept)}>
            {showKept ? 'Back to findings' : `${q.data?.kept.length ?? 0} kept`}
          </Button>
        </Group>
      </Group>

      {unread.length > 0 && !showKept && (
        <Alert color="sky" variant="light" p="sm">
          <Text size={FS.meta}>
            People, access keys, buckets and databases in {unread.map((a) => a.name).join(', ')} have not been read yet, so those checks are not
            made there. They are read with the next sync; if this stays, the account's read role may not reach IAM, S3 or RDS.
          </Text>
        </Alert>
      )}

      {pickedFindings.length > 0 && (
        <Group gap="sm" p="xs" px="md" style={{ background: 'var(--mantine-color-dark-7)', color: 'white', borderRadius: 10 }}>
          <Text size={FS.body}>{pickedFindings.length} snapshot{pickedFindings.length === 1 ? '' : 's'} picked · up to {dollars(pickedFindings.reduce((n, f) => n + (f.monthly ?? 0), 0))} a month</Text>
          <Button size="xs" color="rose" ml="auto" leftSection={<IconTrash size={14} />} onClick={() => setBulk(true)}>Delete {pickedFindings.length}…</Button>
          <Button size="xs" variant="subtle" color="gray" onClick={() => setPicked(new Set())}>Clear</Button>
        </Group>
      )}

      {q.data && shown.length === 0 ? (
        <EmptyState icon={IconCircleCheck} title={showKept ? 'Nothing kept' : 'Nothing found'}
          description={showKept ? 'Findings someone keeps on purpose show here, with who kept them and why.'
            : 'No security risk, nothing paid for and not in use, and no firewall rule to tidy, in what was last read.'} />
      ) : phone ? (
        <Stack gap="sm">
          {shown.map((f) => (
            <Box key={f.accountId + f.id} p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 10, background: 'var(--mantine-color-body)' }}>
              <Group gap="xs"><SeverityBadge s={f.severity} />{f.monthly ? <Text size={FS.meta} fw={600} ml="auto">{f.atMost ? 'up to ' : ''}{dollars(f.monthly)}/mo</Text> : null}</Group>
              <Text fw={600} mt={6} onClick={() => setOpen(f)} style={{ cursor: 'pointer' }}>{f.title}</Text>
              <Text size={FS.meta} c="dimmed">{f.name} · {f.accountName}</Text>
              <Text size={FS.meta} mt={4}>{f.why}</Text>
              <Group mt={8} gap="xs"><SourceTag f={f} />
                <Button size="compact-xs" variant="default" ml="auto" onClick={() => (showKept ? setOpen(f) : fix(f))}>{showKept ? 'Details' : fixLabel(f)}</Button>
              </Group>
            </Box>
          ))}
        </Stack>
      ) : (
        <Box style={{ border: '1px solid var(--color-line)', borderRadius: 10, overflowX: 'auto', background: 'var(--mantine-color-body)' }}>
          <Table miw={980} verticalSpacing="xs">
            <Table.Thead>
              <Table.Tr>
                <Table.Th w={28} /><Table.Th w={84}>How bad</Table.Th><Table.Th>What Argus found</Table.Th><Table.Th w={250}>Where</Table.Th>
                <Table.Th w={140}>How it knows</Table.Th><Table.Th w={100} ta="right">Cost</Table.Th><Table.Th w={190} />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {(['risk', 'waste', 'rules'] as FindingGroup[]).map((g) => {
                const rows = shown.filter((f) => f.group === g)
                if (rows.length === 0) return null
                const money = rows.reduce((n, f) => n + (f.monthly ?? 0), 0)
                return [
                  <Table.Tr key={g} bg="var(--mantine-color-default-hover)">
                    <Table.Td colSpan={7}>
                      <Text size={FS.meta} fw={650}>{GROUP_LABEL[g]}
                        <Text span c="dimmed" fw={400}>{g === 'risk' ? ' · fix these first' : g === 'waste' && money ? ` · ${dollars(money)} a month` : ''}
                          {g === 'rules' ? ' · AWS does not record whether a rule was ever used; these are rules nothing needs' : ''}</Text>
                      </Text>
                    </Table.Td>
                  </Table.Tr>,
                  ...rows.map((f) => (
                    <Table.Tr key={f.accountId + f.id}>
                      <Table.Td>
                        {f.check === 'idle-snapshot' && admin && !showKept && (
                          <Checkbox size="xs" aria-label={`Pick ${f.name}`} checked={picked.has(f.id)} onChange={() => toggle(f)} />
                        )}
                      </Table.Td>
                      <Table.Td><SeverityBadge s={f.severity} /></Table.Td>
                      <Table.Td>
                        <Text size={FS.body} fw={600} onClick={() => setOpen(f)} style={{ cursor: 'pointer' }}>{f.title}</Text>
                        <Text size={FS.meta} c="dimmed" lh={1.4}>{f.why}</Text>
                        {f.kept && <Text size={FS.micro} c="dimmed" mt={2}>Kept by {f.kept.keptBy} {relTime(f.kept.keptAt)}: “{f.kept.reason}”</Text>}
                      </Table.Td>
                      <Table.Td>
                        <Text size={FS.meta} fw={600} style={{ overflowWrap: 'anywhere' }}>{f.name}</Text>
                        <Group gap={4} wrap="nowrap"><Text size={FS.micro} c="dimmed">{f.accountName}{f.region !== 'global' ? ` · ${f.region}` : ''}</Text><EnvBadge env={f.environment} /></Group>
                      </Table.Td>
                      <Table.Td><SourceTag f={f} /></Table.Td>
                      <Table.Td ta="right">
                        {f.monthly ? <><Text size={FS.meta} fw={600}>{f.atMost ? 'up to ' : ''}{dollars(f.monthly)}</Text><Text size={FS.micro} c="dimmed">a month</Text></> : null}
                      </Table.Td>
                      <Table.Td ta="right" style={{ whiteSpace: 'nowrap' }}>
                        {showKept ? (
                          <Button size="compact-xs" variant="subtle" onClick={() => setOpen(f)}>Details</Button>
                        ) : (
                          <>
                            {askedOf(f) ? <Asked c={askedOf(f)!} /> : <Button size="compact-xs" variant="default" onClick={() => fix(f)}>{fixLabel(f)}</Button>}
                            {admin && <Button size="compact-xs" variant="subtle" onClick={() => setKeeping(f)}>Keep it</Button>}
                          </>
                        )}
                      </Table.Td>
                    </Table.Tr>
                  )),
                ]
              })}
            </Table.Tbody>
          </Table>
        </Box>
      )}

      <FindingDrawer f={open} onClose={() => setOpen(null)} admin={admin} hasItem={Boolean(open && (itemOf(open) || capacityOf(open)))}
        asked={open ? askedOf(open) : undefined}
        onFix={(f) => { setOpen(null); fix(f) }} onKeep={(f) => { setOpen(null); setKeeping(f) }} />
      {keeping && <KeepModal f={keeping} onClose={() => setKeeping(null)} />}
      <ResourceDrawer it={resource} onClose={() => setResource(null)} names={names} />
      {rule && <RemoveRuleChange it={rule.it} rule={rule.f.rule!} onClose={() => setRule(null)} />}
      {settings && (
        <SettingsChange it={settings} env={envOf(settings.resource.tags, settings.environment)} preset={{ requireTokens: true }}
          onClose={() => setSettings(null)} />
      )}
      {capacity && <CancelCapacity it={capacity} onClose={() => setCapacity(null)} />}
      {bulk && <BulkSnapshotDelete findings={pickedFindings} items={pickedFindings.map(itemOf)} onClose={(done) => { setBulk(false); if (done) setPicked(new Set()) }} />}
    </Stack>
  )
}

export function SeverityBadge({ s }: { s: FindingSeverity }) {
  return <Badge variant={s === 'low' ? 'light' : 'filled'} color={SEVERITY_COLOR[s]} radius="sm" w={72}>{SEVERITY_LABEL[s]}</Badge>
}

function SourceTag({ f }: { f: CloudFinding }) {
  const color = f.source === 'cloud' ? 'var(--mantine-color-orange-6)' : f.source === 'agent' ? 'var(--mantine-color-teal-6)' : 'var(--mantine-color-gray-5)'
  return (
    <Tooltip label={SOURCE_HELP[f.source]} withArrow multiline w={240}>
      <Group gap={5} wrap="nowrap" style={{ cursor: 'help' }}>
        <Box w={7} h={7} style={{ borderRadius: 7, background: color, flex: 'none' }} />
        <Text size={FS.micro} c="dimmed">{SOURCE_LABEL[f.source]}</Text>
      </Group>
    </Tooltip>
  )
}

/** Where the change already asked for this finding has got to. */
function Asked({ c }: { c: CloudChange }) {
  const words = c.state === 'awaiting_approval' ? 'Waiting for approval' : c.state === 'applying' ? 'Being made' : 'Done · reading it again'
  return (
    <Tooltip label={c.summary} multiline maw={360} withArrow>
      <Badge size="sm" variant="light" color={c.state === 'awaiting_approval' ? 'amber' : 'teal'} style={{ cursor: 'default' }}>{words}</Badge>
    </Tooltip>
  )
}

function FindingDrawer({ f, onClose, admin, hasItem, asked, onFix, onKeep }: {
  f: CloudFinding | null; onClose: () => void; admin: boolean; hasItem: boolean; asked?: CloudChange
  onFix: (f: CloudFinding) => void; onKeep: (f: CloudFinding) => void
}) {
  const unkeep = useUnkeepFinding()
  return (
    <Drawer opened={Boolean(f)} onClose={onClose} position="right" size="lg"
      title={f ? <Stack gap={6}><SeverityBadge s={f.severity} /><Text fw={700} size="lg">{f.title}</Text></Stack> : null}>
      {f && (
        <Stack gap="md">
          <Group gap="xs"><Text size={FS.meta} c="dimmed">{f.name} · {f.accountName}{f.region !== 'global' ? ` · ${f.region}` : ''}</Text><EnvBadge env={f.environment} /></Group>
          {f.matters && <Field label="Why it matters"><Text size={FS.body} lh={1.55}>{f.matters}</Text></Field>}
          <Field label="What Argus read">
            <Text size={FS.body} lh={1.55}>{f.why}</Text>
            <Group mt={6}><SourceTag f={f} /></Group>
          </Field>
          {f.related && f.related.length > 0 && (
            <Field label="Involved">
              <Group gap={6} mt={4}>{f.related.map((r) => <Badge key={r.kind + r.id} variant="default" radius="sm" tt="none" fw={500}>{r.name || r.id}</Badge>)}</Group>
            </Field>
          )}
          {f.monthly ? <Field label="What it bills"><Text size={FS.body}>{f.atMost ? 'Up to ' : ''}{dollars(f.monthly)} a month, at us-east-1 list prices</Text></Field> : null}
          {f.rule && (
            <Field label="Suggested fix">
              <Code block fz={FS.meta} c={INK.denied}>− {ruleText(f.rule, new Map())}</Code>
            </Field>
          )}
          {f.howToFix && <Field label="How to fix"><Text size={FS.body} lh={1.55}>{f.howToFix}</Text></Field>}
          {f.kept ? (
            <Alert color="gray" variant="light" title={`Kept by ${f.kept.keptBy}, ${relTime(f.kept.keptAt)}`}>
              <Text size={FS.meta}>“{f.kept.reason}”</Text>
              <Text size={FS.micro} c="dimmed" mt={4}>
                {f.kept.expiresAt ? `Comes back on ${new Date(f.kept.expiresAt).toLocaleDateString()}` : 'Comes back when it changes'}
                {f.kept.expiresAt && f.kept.untilChange ? ', or sooner if it changes' : ''}.
              </Text>
              {admin && (
                <Button mt="sm" size="xs" variant="default" loading={unkeep.isPending}
                  onClick={() => unkeep.mutate({ accountId: f.accountId, findingId: f.id }, {
                    onSuccess: () => { notifyOk('Listed again', f.title); onClose() },
                    onError: (e) => notifyError('Could not stop keeping it', e),
                  })}>Stop keeping it</Button>
              )}
            </Alert>
          ) : (
            <Group gap="sm" mt="xs">
              {asked && <Text size={FS.meta} c="dimmed">Already asked for: {asked.summary}.</Text>}
              {hasItem && !asked && <Button onClick={() => onFix(f)}>{f.fix === 'firewall-rules' ? 'Ask to remove this rule' : (f.fix && FIX_LABEL[f.fix]) || 'Open the resource'}</Button>}
              {admin && <Button variant="subtle" ml="auto" onClick={() => onKeep(f)}>Keep it…</Button>}
            </Group>
          )}
          <Text size={FS.micro} c="dimmed">Nothing changes from here. A fix opens the usual change, with its preview, approval where the environment needs one, and an audit entry.</Text>
        </Stack>
      )}
    </Drawer>
  )
}

function KeepModal({ f, onClose }: { f: CloudFinding; onClose: () => void }) {
  const keep = useKeepFinding()
  const [reason, setReason] = useState('')
  const [how, setHow] = useState<'7' | '30' | '90' | 'change'>(f.severity === 'high' ? '30' : '90')
  const days: KeepInput['days'] = how === 'change' ? 0 : (Number(how) as 7 | 30 | 90)
  return (
    <Modal opened onClose={onClose} centered size="md" title={<Text fw={700}>Keep “{f.title}”?</Text>}>
      <Stack gap="md">
        <Text size={FS.body} lh={1.55}>
          It stops being listed for {f.name}. Everyone with Cloud access sees that you kept it and why, and it is in Activity.
        </Text>
        <Textarea label="Why" autosize minRows={2} maxLength={300} value={reason} onChange={(e) => setReason(e.currentTarget.value)} data-autofocus />
        <Radio.Group label="For how long" value={how} onChange={(v) => setHow(v as typeof how)}>
          <Stack gap={SP.cozy} mt={6}>
            <Radio value="7" label="7 days" />
            <Radio value="30" label="30 days" />
            <Radio value="90" label="90 days" />
            <Radio value="change" label="Until it changes" disabled={!keepAllowed(f, 0)}
              description={f.severity === 'high' ? 'A High risk is kept for at most 90 days, so it is looked at again.' : 'It comes back when what it rests on changes: a rule, an address, a size.'} />
          </Stack>
        </Radio.Group>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={keep.isPending} disabled={reason.trim().length < 3}
            onClick={() => keep.mutate({ accountId: f.accountId, findingId: f.id, reason: reason.trim(), days, untilChange: how === 'change' }, {
              onSuccess: () => { notifyOk('Kept', f.title); onClose() },
              onError: (e) => notifyError('Could not keep it', e),
            })}>Keep it</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/** Several old snapshots at once: each one its own change, with its own approval and audit entry. */
function BulkSnapshotDelete({ findings, items, onClose }: { findings: CloudFinding[]; items: (CloudItem | undefined)[]; onClose: (done: boolean) => void }) {
  const ask = useAskCloudChange()
  const [typed, setTyped] = useState('')
  const [reason, setReason] = useState('')
  const [busy, setBusy] = useState(false)
  const n = findings.length
  const word = `delete ${n} snapshot${n === 1 ? '' : 's'}`
  const approval = findings.some((f) => needsApproval(f.environment, 'remove'))
  const missing = items.some((it) => !it)
  const go = async () => {
    setBusy(true)
    let ok = 0
    for (const it of items) {
      if (!it) continue
      try {
        await ask.mutateAsync({ accountId: it.accountId, region: it.resource.region, action: 'snapshot-delete', resourceId: it.resource.id, confirm: it.resource.id, reason })
        ok++
      } catch (err) {
        notifyError(`Could not ask to delete ${it.resource.name}`, err)
      }
    }
    setBusy(false)
    if (ok) notifyOk(approval ? `Asked to delete ${ok} snapshot${ok === 1 ? '' : 's'}` : `Deleting ${ok} snapshot${ok === 1 ? '' : 's'}`,
      approval ? 'Each waits for another approver.' : 'Each is its own change in Changes.')
    onClose(ok > 0)
  }
  return (
    <Modal opened onClose={() => onClose(false)} centered size="lg" title={<Text fw={700}>Delete {n} snapshot{n === 1 ? '' : 's'}</Text>}>
      <Stack gap="md">
        <Text size={FS.body} lh={1.55}>Each one is a snapshot of a disk that no longer exists, older than 30 days, and no image in the account uses it.</Text>
        <Box style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
          <Table verticalSpacing={6}>
            <Table.Tbody>
              {findings.map((f) => (
                <Table.Tr key={f.id}>
                  <Table.Td><Text size={FS.meta} ff="monospace">{f.resourceId}</Text><Text size={FS.micro} c="dimmed">{f.name !== f.resourceId ? f.name : ''}</Text></Table.Td>
                  <Table.Td><Text size={FS.meta}>{f.accountName}</Text></Table.Td>
                  <Table.Td ta="right"><Text size={FS.meta}>up to {dollars(f.monthly ?? 0)}</Text></Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Box>
        <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />} title="A deleted snapshot cannot be brought back">
          <Text size={FS.meta}>
            AWS bills only the blocks a snapshot does not share with others, so the saving may be less than shown.
            {approval ? ' Some are in an environment that needs approval, so those deletes wait for another approver.' : ''}
          </Text>
        </Alert>
        {missing && <Text size={FS.meta} c={INK.denied}>Some of these are no longer in the last read; they are skipped.</Text>}
        {approval && <Textarea label="Why" description="The approver sees this." autosize minRows={2} value={reason} onChange={(e) => setReason(e.currentTarget.value)} />}
        <TextInput label={<>Type <Text span ff="monospace" fw={700}>{word}</Text> to confirm</>} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} />
        <Group justify="flex-end">
          <Button variant="default" onClick={() => onClose(false)}>Cancel</Button>
          <Button color="rose" loading={busy} disabled={typed.trim() !== word || (approval && reason.trim().length < 5)} onClick={() => void go()}>
            {approval ? `Ask to ${word}` : word[0]!.toUpperCase() + word.slice(1)}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
