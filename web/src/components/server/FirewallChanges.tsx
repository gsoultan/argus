import {
  Alert, Anchor, Badge, Box, Button, Card, Code, Collapse, Drawer, Group, Modal, MultiSelect, ScrollArea, SegmentedControl,
  Select, Skeleton, Stack, Switch, Table, TagsInput, Text, Textarea, TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { AnchorLink } from '~/components/links'
import { IconCheck, IconPlus, IconRestore, IconShieldLock, IconTrash, IconX } from '@tabler/icons-react'
import { Fragment, useState } from 'react'
import { ApproveOwn } from '~/components/ApproveOwn'
import { SectionCard } from '~/components/page'
import { absTime, relTime } from '~/components/primitives'
import {
  changeOpen, changeStateColor, changeStateWords, lineWhat, lineWords, planFirewall, sourceWords,
  type FirewallAsk, type FirewallChange, type FirewallManage, type FWLine, type FWSource, type FWSourceKind,
} from '~/lib/firewallApi'
import {
  firewallPoliciesQuery, useActOnFirewallChange, useAskFirewallChange, useSetFirewallManagers,
} from '~/lib/firewallQueries'
import { run as notifyRun } from '~/lib/notify'
import { assetsQuery, meQuery } from '~/lib/queries'
import { useDecidesAlone } from '~/lib/selfApproval'
import { FS, SP } from '~/theme'
import type { Asset } from '~/types/domain'

/**
 * Changing a server's firewall from its Firewall tab
 * (plans/server-firewall/design.md): the lines Argus keeps, the change on
 * its way, what came before, and who may change it.
 *
 * Every change is reviewed first, with the commands the agent will run, and
 * is a trial on the server: it undoes itself unless the server still
 * reaches Argus two minutes later. Turning a firewall on also waits for a
 * person to press Keep.
 */
export function FirewallManagePanel({ asset, manage: m, kind }: { asset: Asset; manage: FirewallManage; kind: string }) {
  const { data: me } = useQuery(meQuery())
  const [adding, setAdding] = useState(false)
  const admin = me?.role === 'admin' || me?.role === 'owner'
  const can = m.canChange && m.ready
  return (
    <Stack gap="md">
      {m.open && <OpenChange change={m.open} canChange={m.canChange} />}
      {m.canChange && !m.ready && m.reason && (
        <Alert color="slate" title="Argus cannot change this firewall yet" data-testid="firewall-not-ready">
          <Text size={FS.body}>{m.reason}</Text>
        </Alert>
      )}
      <SectionCard
        title="Lines Argus keeps"
        description="Argus adds these to the firewall and keeps them there. Rules made by hand stay as they are unless you take them over."
        flush
        action={can ? (
          <Button size="compact-sm" leftSection={<IconPlus size={14} />} onClick={() => setAdding(true)} data-testid="firewall-add">
            Let something in
          </Button>
        ) : undefined}
      >
        <Box data-testid="firewall-own">
          <OwnLines asset={asset} manage={m} can={can} />
        </Box>
      </SectionCard>
      {m.policies.length > 0 && <PoliciesHere manage={m} />}
      {m.changes.length > 0 && <History asset={asset} manage={m} can={can} />}
      {admin && <Managers asset={asset} managers={m.managers} />}
      {adding && <LineDrawer asset={asset} manage={m} ufw={kind === 'ufw' || (!kind && m.install === 'ufw')} onClose={() => setAdding(false)} />}
    </Stack>
  )
}

/* ── The change on its way ───────────────────────────────────────────────── */

function OpenChange({ change: c, canChange }: { change: FirewallChange; canChange: boolean }) {
  const { data: me } = useQuery(meQuery())
  const act = useActOnFirewallChange()
  const alone = useDecidesAlone('runs')
  const [show, setShow] = useState(false)
  const mine = me?.email?.toLowerCase() === c.requestedBy.toLowerCase()
  const admin = me?.role === 'admin' || me?.role === 'owner'
  const what: Record<string, string> = {
    awaiting_approval: 'Another admin approves this change before the server is asked to make it.',
    queued: 'The server makes it at its next check-in, within a minute.',
    sent: 'The server is saving its firewall and making the change.',
    trial: 'The change is made. If the server cannot reach Argus within two minutes, it puts the saved copy back by itself.',
    waiting_keep: 'The change is made and the server still reaches Argus. Check you can still reach the server, then keep it; otherwise it is undone.',
  }
  const keepLeft = c.keepBefore ? Math.max(0, Math.round((new Date(c.keepBefore).getTime() - Date.now()) / 60000)) : 0
  const go = (action: 'approve' | 'deny' | 'cancel' | 'keep', ok: [string, string], failure: string) =>
    void notifyRun(() => act.mutateAsync({ id: c.id, action }), { failure, success: ok })
  return (
    <Card padding="md" withBorder data-testid="firewall-open-change"
      style={{ borderColor: `var(--mantine-color-${changeStateColor(c.state)}-4)` }}>
      <Stack gap={SP.cozy}>
        <Group gap={SP.snug} justify="space-between" wrap="nowrap" align="flex-start">
          <Box style={{ minWidth: 0 }}>
            <Group gap={SP.snug}>
              <Badge variant="light" radius="sm" tt="none" color={changeStateColor(c.state)}>{changeStateWords(c.state)}</Badge>
              <Text fw={650} size="sm">{c.summary}</Text>
            </Group>
            <Text size={FS.meta} c="dimmed" mt={2}>
              Asked by {c.requestedBy} {relTime(c.requestedAt)}{c.reason ? ` · ${c.reason}` : ''}
              {c.decidedBy ? ` · approved by ${c.decidedBy}` : ''}
            </Text>
            <Text size={FS.body} mt={SP.tight} lh={1.5}>{what[c.state]}</Text>
            {c.state === 'waiting_keep' && !c.keptAt && (
              <Text size={FS.meta} c="amber.8" mt={2}>Undone in about {keepLeft} minute{keepLeft === 1 ? '' : 's'} unless kept.</Text>
            )}
            {c.keptAt && <Text size={FS.meta} c="dimmed" mt={2}>Kept by {c.keptBy}; the server hears it at its next check-in.</Text>}
          </Box>
          <Group gap={SP.snug} wrap="nowrap">
            {c.state === 'waiting_keep' && !c.keptAt && canChange && (
              <Button size="compact-sm" color="teal" leftSection={<IconCheck size={14} />} loading={act.isPending}
                data-testid="firewall-keep"
                onClick={() => go('keep', ['Kept', 'The server keeps the change.'], 'Not kept')}>
                Keep
              </Button>
            )}
            {c.state === 'awaiting_approval' && admin && mine && alone && (
              <ApproveOwn what="this firewall change" loading={act.isPending}
                onApprove={(confirmSelf) => notifyRun(() => act.mutateAsync({ id: c.id, action: 'approve', confirmSelf }),
                  { failure: 'Not approved', success: ['Approved', 'The server makes it at its next check-in.'] })} />
            )}
            {c.state === 'awaiting_approval' && admin && !mine && (
              <>
                <Button size="compact-sm" color="teal" leftSection={<IconCheck size={14} />} loading={act.isPending}
                  onClick={() => go('approve', ['Approved', 'The server makes it at its next check-in.'], 'Not approved')}>
                  Approve
                </Button>
                <Button size="compact-sm" variant="default" color="rose" leftSection={<IconX size={14} />} loading={act.isPending}
                  onClick={() => go('deny', ['Denied', 'Nothing is changed.'], 'Not denied')}>
                  Deny
                </Button>
              </>
            )}
            {(c.state === 'awaiting_approval' || c.state === 'queued') && canChange && (mine || admin) && (
              <Button size="compact-sm" variant="default" loading={act.isPending}
                onClick={() => go('cancel', ['Taken back', 'Nothing was changed.'], 'Not taken back')}>
                Take back
              </Button>
            )}
          </Group>
        </Group>
        {c.preview.length > 0 && (
          <Box>
            <Anchor component="button" size={FS.meta} onClick={() => setShow((s) => !s)}>
              {show ? 'Hide' : 'Show'} the commands
            </Anchor>
            <Collapse expanded={show}><Commands lines={c.preview} /></Collapse>
          </Box>
        )}
      </Stack>
    </Card>
  )
}

function Commands({ lines }: { lines: string[] }) {
  return (
    <ScrollArea.Autosize mah={240} mt={SP.tight}>
      <Code block fz={FS.micro} data-testid="firewall-commands">{lines.join('\n')}</Code>
    </ScrollArea.Autosize>
  )
}

/* ── Lines Argus keeps ───────────────────────────────────────────────────── */

function OwnLines({ asset, manage: m, can }: { asset: Asset; manage: FirewallManage; can: boolean }) {
  const [removing, setRemoving] = useState<FWLine | null>(null)
  return (
    <>
      <Table verticalSpacing="sm" horizontalSpacing="md">
        <Table.Thead>
          <Table.Tr>
            <Table.Th>Line</Table.Th>
            <Table.Th>For</Table.Th>
            <Table.Th>Note</Table.Th>
            <Table.Th />
          </Table.Tr>
        </Table.Thead>
        <Table.Tbody>
          {m.own.length === 0 && (
            <Table.Tr>
              <Table.Td colSpan={4}>
                <Text size={FS.body} c="dimmed">
                  {can ? 'None yet. Let something in, or take over a rule made by hand from the list above.' : 'None.'}
                </Text>
              </Table.Td>
            </Table.Tr>
          )}
          {m.own.map((l) => (
            <Table.Tr key={l.key}>
              <Table.Td>
                <Group gap={SP.tight} wrap="nowrap">
                  {l.action === 'deny' && <Badge size="xs" variant="light" color="rose" radius="sm" tt="none">Blocks</Badge>}
                  {l.direction === 'out' && <Badge size="xs" variant="light" color="slate" radius="sm" tt="none">Going out</Badge>}
                  <Text size="sm" fw={600}>{lineWhat(l)}</Text>
                </Group>
              </Table.Td>
              <Table.Td><Text size="sm">{l.from.map(sourceWords).join(', ')}</Text></Table.Td>
              <Table.Td>
                <Text size={FS.meta} c="dimmed">
                  {l.note || '—'}
                  {l.until ? ` · ends ${relTime(l.until)}` : ''}
                  {l.addedBy ? ` · ${l.addedBy}` : ''}
                </Text>
              </Table.Td>
              <Table.Td ta="right">
                {can && !m.open && (
                  <Button size="compact-xs" variant="subtle" color="rose" leftSection={<IconTrash size={12} />}
                    onClick={() => setRemoving(l)}>
                    Remove
                  </Button>
                )}
              </Table.Td>
            </Table.Tr>
          ))}
          <Table.Tr>
            <Table.Td colSpan={4} bg="var(--mantine-color-body)">
              <Text size={FS.meta} c="dimmed">
                {m.defaultOut === 'deny'
                  ? `Going out is blocked but for these lines and what Argus always allows: ${m.always.join('; ')}.`
                  : 'Going out is as the firewall had it.'}
              </Text>
            </Table.Td>
          </Table.Tr>
        </Table.Tbody>
      </Table>
      {removing && (
        <ReviewModal asset={asset} manage={m} title={`Remove: ${lineWords(removing)}`} ask={{ remove: [removing.key!] }}
          onClose={() => setRemoving(null)} />
      )}
    </>
  )
}

function PoliciesHere({ manage: m }: { manage: FirewallManage }) {
  return (
    <SectionCard title="Firewall policies on this server"
      description="Shared lines from Assets › Firewalls › Policies. Change them there; every server with the policy follows."
      flush>
      <Table verticalSpacing="sm" horizontalSpacing="md" data-testid="firewall-policies-here">
        <Table.Tbody>
          {m.policies.map((p) => (
            <Table.Tr key={p.id}>
              <Table.Td>
                <AnchorLink to="/assets/firewall-policies/$policyId" params={{ policyId: p.id }} size="sm" fw={600}>
                  {p.name}
                </AnchorLink>
              </Table.Td>
              <Table.Td>
                <Text size={FS.meta} c="dimmed">
                  {p.version === 0 ? 'On its way' : `Version ${p.version}`}
                  {p.version > 0 && p.latest > p.version ? ` · version ${p.latest} on its way` : ''}
                  {!p.wanted ? ' · being taken off' : ''}
                </Text>
              </Table.Td>
              <Table.Td><Text size={FS.meta} c="dimmed">{p.lines.map(lineWords).join('; ') || 'No lines'}</Text></Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
    </SectionCard>
  )
}

/* ── History ─────────────────────────────────────────────────────────────── */

function History({ asset, manage: m, can }: { asset: Asset; manage: FirewallManage; can: boolean }) {
  const [open, setOpen] = useState<string | null>(null)
  const [putBack, setPutBack] = useState<FirewallChange | null>(null)
  const past = m.changes.filter((c) => !changeOpen(c.state))
  if (past.length === 0) return null
  return (
    <SectionCard title="Changes" description="Each change saved the firewall first. Put back returns it to how it was before that change." flush>
      <Table verticalSpacing="xs" horizontalSpacing="md" data-testid="firewall-history">
        <Table.Tbody>
          {past.map((c) => (
            <Fragment key={c.id}>
              <Table.Tr style={{ cursor: 'pointer' }} onClick={() => setOpen(open === c.id ? null : c.id)}>
                <Table.Td w={120}>
                  <Badge size="sm" variant="light" radius="sm" tt="none" color={changeStateColor(c.state)}>{changeStateWords(c.state)}</Badge>
                </Table.Td>
                <Table.Td>
                  <Text size="sm">{c.summary}</Text>
                  <Text size={FS.micro} c="dimmed" title={absTime(c.requestedAt)}>
                    {c.requestedBy === 'argus' ? 'Argus' : c.requestedBy} · {relTime(c.requestedAt)}
                    {c.message && c.state !== 'applied' ? ` · ${c.message}` : ''}
                  </Text>
                </Table.Td>
                <Table.Td ta="right">
                  {can && c.canPutBack && !m.open && (
                    <Button size="compact-xs" variant="subtle" leftSection={<IconRestore size={12} />}
                      onClick={(e) => { e.stopPropagation(); setPutBack(c) }}>
                      Put back
                    </Button>
                  )}
                </Table.Td>
              </Table.Tr>
              {open === c.id && (
                <Table.Tr>
                  <Table.Td colSpan={3}>
                    {c.ran.length > 0
                      ? <Commands lines={c.ran.map((r) => `${r.ok ? '✓' : '✗'} ${r.command}${r.output ? `\n    ${r.output}` : ''}`)} />
                      : c.preview.length > 0 ? <Commands lines={c.preview} /> : <Text size={FS.meta} c="dimmed">Nothing ran.</Text>}
                  </Table.Td>
                </Table.Tr>
              )}
            </Fragment>
          ))}
        </Table.Tbody>
      </Table>
      {putBack && (
        <ReviewModal asset={asset} manage={m} title="Put back the firewall from before a change" ask={{ putBack: putBack.id }}
          onClose={() => setPutBack(null)} />
      )}
    </SectionCard>
  )
}

/* ── Who may change it ───────────────────────────────────────────────────── */

function Managers({ asset, managers }: { asset: Asset; managers: string[] }) {
  const [value, setValue] = useState(managers)
  const save = useSetFirewallManagers(asset.id)
  const changed = value.join(',') !== managers.join(',')
  return (
    <Card padding="md">
      <Group align="flex-end" gap="sm" wrap="nowrap">
        <TagsInput style={{ flex: 1 }} label="Operators who may change this firewall"
          description="Admins and owners always may. An operator's change waits for an admin to approve it."
          placeholder="Email, then Enter" value={value} onChange={setValue} data-testid="firewall-managers" />
        <Button variant="default" disabled={!changed} loading={save.isPending}
          onClick={() => void notifyRun(() => save.mutateAsync(value), { failure: 'Not saved', success: ['Saved', 'Who may change this firewall is updated.'] })}>
          Save
        </Button>
      </Group>
    </Card>
  )
}

/* ── Asking for a change ─────────────────────────────────────────────────── */

const WHAT: { value: string; label: string; protocol: FWLine['protocol']; ports?: string }[] = [
  { value: 'ssh', label: 'SSH (22)', protocol: 'tcp', ports: '22' },
  { value: 'web', label: 'Web (80, 443)', protocol: 'tcp', ports: '80,443' },
  { value: 'rdp', label: 'Remote Desktop (3389)', protocol: 'tcp', ports: '3389' },
  { value: 'postgres', label: 'Postgres (5432)', protocol: 'tcp', ports: '5432' },
  { value: 'mysql', label: 'MySQL (3306)', protocol: 'tcp', ports: '3306' },
  { value: 'redis', label: 'Redis (6379)', protocol: 'tcp', ports: '6379' },
  { value: 'metrics', label: 'Metrics (9100)', protocol: 'tcp', ports: '9100' },
  { value: 'ping', label: 'Ping', protocol: 'icmp' },
  { value: 'any', label: 'Everything', protocol: 'any' },
  { value: 'tcp', label: 'Other TCP ports…', protocol: 'tcp' },
  { value: 'udp', label: 'Other UDP ports…', protocol: 'udp' },
]

const FOR: { value: FWSourceKind; label: string }[] = [
  { value: 'private', label: 'Private networks' },
  { value: 'address', label: 'Addresses' },
  { value: 'server', label: 'Servers' },
  { value: 'tag', label: 'A tag' },
  { value: 'policy', label: 'A policy' },
  { value: 'everyone', label: 'Everyone' },
]

const HOW_LONG = [
  { value: '', label: 'Until removed' },
  { value: '1', label: '1 hour' },
  { value: '24', label: '1 day' },
  { value: '168', label: '7 days' },
]

/** The line editor, shared with the policy page. */
export function LineForm({ value: l, onChange, ufw, policyId }: {
  value: FWLine
  onChange: (l: FWLine) => void
  /** ufw cannot let ping in line by line. */
  ufw?: boolean
  /** The policy being edited, which a line cannot be for. */
  policyId?: string
}) {
  const what = WHAT.find((w) => w.protocol === l.protocol && (w.ports ?? '') === (l.ports ?? '') && w.value !== 'tcp' && w.value !== 'udp')?.value
    ?? (l.protocol === 'udp' ? 'udp' : 'tcp')
  const kind = l.from[0]?.kind ?? 'private'
  const { data: assets } = useQuery({ ...assetsQuery(), enabled: kind === 'server' })
  const { data: policies } = useQuery({ ...firewallPoliciesQuery(), enabled: kind === 'policy' })
  const setFrom = (from: FWSource[]) => onChange({ ...l, from })
  const custom = what === 'tcp' || what === 'udp'
  return (
    <Stack gap="sm">
      <SegmentedControl fullWidth value={`${l.direction}-${l.action}`} aria-label="Kind of line"
        onChange={(v) => { const [d, a] = v.split('-'); onChange({ ...l, direction: d as FWLine['direction'], action: a as FWLine['action'] }) }}
        data={[
          { value: 'in-allow', label: 'Let in' },
          { value: 'in-deny', label: 'Keep out' },
          { value: 'out-allow', label: 'Let out' },
          { value: 'out-deny', label: 'Block going out' },
        ]} />
      <Select label="What" value={what} allowDeselect={false}
        data={WHAT.filter((w) => !(ufw && w.value === 'ping')).map((w) => ({ value: w.value, label: w.label }))}
        onChange={(v) => {
          const w = WHAT.find((x) => x.value === v)
          if (w) onChange({ ...l, protocol: w.protocol, ports: w.ports ?? (w.value === 'tcp' || w.value === 'udp' ? '' : undefined) })
        }} />
      {custom && (
        <TextInput label="Ports" placeholder="8080, or 6000-6010, or 80,443" value={l.ports ?? ''}
          onChange={(e) => onChange({ ...l, ports: e.currentTarget.value })} />
      )}
      <Select label={l.direction === 'out' ? 'To' : 'From'} value={kind} allowDeselect={false}
        data={FOR}
        onChange={(v) => setFrom([{ kind: v as FWSourceKind }])} />
      {kind === 'address' && (
        <TagsInput label="Addresses or networks" placeholder="10.0.4.7 or 10.0.4.0/24, then Enter"
          value={l.from.map((s) => s.value ?? '').filter(Boolean)}
          onChange={(vs) => setFrom(vs.map((value) => ({ kind: 'address', value })))} />
      )}
      {kind === 'server' && (
        <MultiSelect label="Servers" searchable placeholder="Pick servers"
          description="Their addresses as they report them; a server that moves is followed within the hour."
          data={(assets ?? []).filter((a) => a.kind === 'server' || a.kind === 'workstation').map((a) => ({ value: a.id, label: a.hostname }))}
          value={l.from.map((s) => s.value ?? '').filter(Boolean)}
          onChange={(vs) => setFrom(vs.map((value) => ({ kind: 'server', value })))} />
      )}
      {kind === 'tag' && (
        <TextInput label="Tag" placeholder="web" value={l.from[0]?.value ?? ''}
          description="Every server with the tag, now and later."
          onChange={(e) => setFrom([{ kind: 'tag', value: e.currentTarget.value }])} />
      )}
      {kind === 'policy' && (
        <Select label="Servers with the policy" searchable placeholder="Pick a policy"
          data={(policies?.policies ?? []).filter((p) => p.id !== policyId).map((p) => ({ value: p.id, label: p.name }))}
          value={l.from[0]?.value ?? null}
          onChange={(v) => setFrom([{ kind: 'policy', value: v ?? '' }])} />
      )}
      {kind === 'everyone' && l.direction === 'in' && l.action === 'allow' && (
        <Alert color="amber" p="xs"><Text size={FS.body}>Everyone includes the whole internet. This waits for another admin to approve it.</Text></Alert>
      )}
      <TextInput label="Note" placeholder="What it is for" value={l.note ?? ''} maxLength={200}
        onChange={(e) => onChange({ ...l, note: e.currentTarget.value })} />
    </Stack>
  )
}

export const emptyLine = (): FWLine => ({ direction: 'in', action: 'allow', protocol: 'tcp', ports: '22', from: [{ kind: 'private' }] })

function LineDrawer({ asset, manage: m, ufw, onClose }: { asset: Asset; manage: FirewallManage; ufw: boolean; onClose: () => void }) {
  const [line, setLine] = useState<FWLine>(emptyLine())
  const [hours, setHours] = useState('')
  const [review, setReview] = useState(false)
  const ask: FirewallAsk = {
    add: [{ ...line, until: hours ? new Date(Date.now() + Number(hours) * 3600_000).toISOString() : undefined }],
  }
  return (
    <Drawer opened onClose={onClose} position="right" size="md" title={<Text fw={700}>Let something in on {asset.hostname}</Text>}>
      {review ? (
        <Review asset={asset} manage={m} ask={ask} onBack={() => setReview(false)} onDone={onClose} />
      ) : (
        <Stack gap="md" data-testid="firewall-line-form">
          <LineForm value={line} onChange={setLine} ufw={ufw} />
          <Select label="How long" data={HOW_LONG} value={hours} allowDeselect={false} onChange={(v) => setHours(v ?? '')}
            description="A line with an end comes off by itself at the server's first check-in after." />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button onClick={() => setReview(true)} data-testid="firewall-review">Review</Button>
          </Group>
        </Stack>
      )}
    </Drawer>
  )
}

/** A change in a modal: taking over, removing, putting back, turning on. */
export function ReviewModal({ asset, manage, title, ask, onClose }: {
  asset: Asset
  manage: FirewallManage
  title: string
  ask: FirewallAsk
  onClose: () => void
}) {
  return (
    <Modal opened onClose={onClose} size="lg" title={<Text fw={700}>{title}</Text>}>
      <Review asset={asset} manage={manage} ask={ask} onDone={onClose} onBack={onClose} backLabel="Cancel" />
    </Modal>
  )
}

/**
 * What a change would do, from the control plane: in words, its warnings,
 * whether it waits for approval or for Keep, and the commands themselves.
 */
function Review({ asset, ask, onBack, onDone, backLabel = 'Back' }: {
  asset: Asset
  manage: FirewallManage
  ask: FirewallAsk
  onBack: () => void
  onDone: () => void
  backLabel?: string
}) {
  const key = JSON.stringify(ask)
  const plan = useQuery({
    queryKey: ['firewall-plan', asset.id, key],
    queryFn: () => planFirewall(asset.id, ask),
    retry: false,
    gcTime: 0,
  })
  const send = useAskFirewallChange(asset.id)
  const [reason, setReason] = useState('')
  const [confirm, setConfirm] = useState('')
  const p = plan.data
  if (plan.isLoading) return <Skeleton h={200} />
  if (plan.error || !p) {
    return (
      <Stack gap="md">
        <Alert color="rose" title="This change cannot be made">
          <Text size={FS.body}>{plan.error instanceof Error ? plan.error.message : 'The control plane did not answer.'}</Text>
        </Alert>
        <Group justify="flex-end"><Button variant="default" onClick={onBack}>{backLabel}</Button></Group>
      </Stack>
    )
  }
  const confirmed = !p.needsConfirm || confirm.trim().toLowerCase() === asset.hostname.toLowerCase()
  return (
    <Stack gap="md" data-testid="firewall-plan">
      <Text size="sm" fw={600}>{p.summary}</Text>
      {p.warnings.map((w) => (
        <Alert key={w} color="amber" p="xs"><Text size={FS.body}>{w}</Text></Alert>
      ))}
      <Box>
        <Text size={FS.meta} c="dimmed" mb={SP.tight}>What the server will run</Text>
        <Commands lines={p.preview.length > 0 ? p.preview : ['(nothing changes on the server)']} />
      </Box>
      <Card padding="sm" withBorder>
        <Stack gap={SP.tight}>
          <Group gap={SP.snug} wrap="nowrap"><IconShieldLock size={16} />
            <Text size={FS.body}>The server saves its firewall first, and puts it back by itself if it cannot reach Argus two minutes after the change.</Text>
          </Group>
          {p.keep && <Text size={FS.body}>Then it waits up to ten minutes for you to press Keep, after you check you can still reach it.</Text>}
          {p.needsApproval && <Text size={FS.body} fw={600}>It waits for another admin to approve it.</Text>}
        </Stack>
      </Card>
      {p.needsConfirm && (
        <Alert color="rose" p="sm">
          <Text size={FS.body} mb="xs">{p.needsConfirm}</Text>
          <TextInput placeholder={asset.hostname} value={confirm} onChange={(e) => setConfirm(e.currentTarget.value)}
            aria-label="Type the server's name" />
        </Alert>
      )}
      <Textarea label="Why" placeholder="Shown in Activity and to whoever approves it" autosize minRows={2} maxLength={500}
        value={reason} onChange={(e) => setReason(e.currentTarget.value)} />
      <Group justify="flex-end">
        <Button variant="default" onClick={onBack}>{backLabel}</Button>
        <Button disabled={!confirmed} loading={send.isPending} data-testid="firewall-send"
          onClick={() => void notifyRun(() => send.mutateAsync({ ...ask, reason, confirm }), {
            failure: 'Not changed',
            success: p.needsApproval
              ? ['Asked', 'Another admin approves it before the server makes it.']
              : ['On its way', 'The server makes the change at its next check-in.'],
          }).then((ok) => { if (ok) onDone() })}>
          {p.needsApproval ? 'Ask for approval' : 'Make the change'}
        </Button>
      </Group>
    </Stack>
  )
}

/* ── Turning a firewall on ───────────────────────────────────────────────── */

export function TurnOnButton({ asset, manage: m, kind }: { asset: Asset; manage: FirewallManage; kind: string }) {
  const [open, setOpen] = useState(false)
  const [keepRemote, setKeepRemote] = useState(true)
  const [step, setStep] = useState<'choose' | 'review'>('choose')
  if (!m.canChange || !m.ready || m.open) return null
  const remote: FWLine = {
    direction: 'in', action: 'allow', protocol: 'tcp', ports: String(m.remotePort), from: [{ kind: 'private' }],
    note: m.remotePort === 3389 ? 'Remote Desktop' : 'SSH',
  }
  const ask: FirewallAsk = { turnOn: true, install: kind ? undefined : m.install, add: keepRemote ? [remote] : [] }
  const close = () => { setOpen(false); setStep('choose') }
  return (
    <>
      <Button size="compact-sm" color="teal" onClick={() => setOpen(true)} data-testid="firewall-turn-on">
        {kind ? 'Turn on' : `Install ${m.install} and turn on`}
      </Button>
      <Modal opened={open} onClose={close} size="lg" title={<Text fw={700}>Turn the firewall on for {asset.hostname}</Text>}>
        {step === 'review' ? (
          <Review asset={asset} manage={m} ask={ask} onBack={() => setStep('choose')} onDone={close} />
        ) : (
          <Stack gap="md">
            <Text size={FS.body} lh={1.5}>
              Once on, the firewall blocks everything coming in that no line lets in. The server&apos;s connection to Argus
              goes out, so Argus keeps reaching it. The change is a trial: it is undone by itself unless you press Keep
              within ten minutes.
            </Text>
            <Switch checked={keepRemote} onChange={(e) => setKeepRemote(e.currentTarget.checked)}
              label={`Keep ${m.remotePort === 3389 ? 'Remote Desktop' : 'SSH'} (${m.remotePort}) open from private networks`} />
            <Group justify="flex-end">
              <Button variant="default" onClick={close}>Cancel</Button>
              <Button onClick={() => setStep('review')}>Review</Button>
            </Group>
          </Stack>
        )}
      </Modal>
    </>
  )
}

/** Take a rule made by hand over: it becomes a line Argus keeps. */
export function TakeOverButton({ asset, manage: m, raw }: { asset: Asset; manage: FirewallManage; raw: string }) {
  const [open, setOpen] = useState(false)
  if (!m.canChange || !m.ready || m.open) return null
  return (
    <>
      <Button size="compact-xs" variant="subtle" onClick={() => setOpen(true)} title="Argus keeps it from now on">
        Take over
      </Button>
      {open && (
        <ReviewModal asset={asset} manage={m} title="Take over a rule made by hand" ask={{ takeOver: [raw] }}
          onClose={() => setOpen(false)} />
      )}
    </>
  )
}
