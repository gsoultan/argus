import {
  Alert, Badge, Box, Button, Group, Modal, Progress, SegmentedControl, Select, Skeleton, Stack, Table, Text,
  Textarea, TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { AnchorLink } from '~/components/links'
import { createFileRoute, useNavigate } from '@tanstack/react-router'
import { IconCheck, IconPencil, IconPlayerStop, IconPlus, IconTrash, IconX } from '@tabler/icons-react'
import { useEffect, useState } from 'react'
import { ApproveOwn } from '~/components/ApproveOwn'
import { PageBody, PageHeader, SectionCard } from '~/components/page'
import { emptyLine, LineForm } from '~/components/server/FirewallChanges'
import {
  changeStateColor, changeStateWords, lineWords, type FirewallPolicy, type FWLine, type FWRollout,
} from '~/lib/firewallApi'
import {
  firewallPolicyQuery, useActOnFirewallRollout, useDeleteFirewallPolicy, useFirewallTarget, useSaveFirewallPolicy,
} from '~/lib/firewallQueries'
import { guard } from '~/lib/guard'
import { isConfigured } from '~/lib/live'
import { run as notifyRun } from '~/lib/notify'
import { assetsQuery, meQuery } from '~/lib/queries'
import { useDecidesAlone } from '~/lib/selfApproval'
import { FS, SP } from '~/theme'

export const Route = createFileRoute('/assets/firewall-policies/$policyId')({
  beforeLoad: ({ context }) => guard(context.queryClient, 'fleet'),
  component: PolicyPage,
})

/**
 * One firewall policy: its lines, where it is, and its rollout. Saving
 * changed lines makes a new version and rolls it out: one server first, then
 * five at a time, stopping at the first that fails.
 */
function PolicyPage() {
  const { policyId } = Route.useParams()
  const { data: p, isLoading, error } = useQuery(firewallPolicyQuery(policyId))
  const { data: me } = useQuery(meQuery())
  const admin = me?.role === 'admin' || me?.role === 'owner'
  return (
    <Box>
      <PageHeader
        crumbs={[{ label: 'Assets', to: '/assets' }, { label: 'Firewalls', to: '/assets/firewalls' },
          { label: 'Policies', to: '/assets/firewall-policies' }, { label: p?.name ?? 'Policy' }]}
        title={p?.name ?? 'Firewall policy'}
        status={p ? <Badge variant="light" color="slate" radius="sm" tt="none">Version {p.version}</Badge> : undefined}
        description={p?.description || undefined}
      />
      <PageBody>
        {!isConfigured() ? (
          <Alert color="slate"><Text size={FS.body}>Connect this console to Argus to see firewall policies.</Text></Alert>
        ) : error ? (
          <Alert color="rose"><Text size={FS.body}>Could not read the policy: {error.message}</Text></Alert>
        ) : isLoading || !p ? (
          <Skeleton h={300} />
        ) : (
          <>
            {p.rollout && <RolloutCard rollout={p.rollout} admin={admin} />}
            <LinesCard p={p} admin={admin} />
            <TargetsCard p={p} admin={admin} />
            {admin && <DeleteCard p={p} />}
          </>
        )}
      </PageBody>
    </Box>
  )
}

function RolloutCard({ rollout: r, admin }: { rollout: FWRollout; admin: boolean }) {
  const { data: me } = useQuery(meQuery())
  const act = useActOnFirewallRollout()
  const alone = useDecidesAlone('runs')
  const mine = me?.email?.toLowerCase() === r.requestedBy.toLowerCase()
  const go = (action: 'approve' | 'deny' | 'stop', ok: [string, string], failure: string) =>
    void notifyRun(() => act.mutateAsync({ id: r.id, action }), { failure, success: ok })
  const words: Record<FWRollout['state'], string> = {
    awaiting_approval: 'Waiting for approval', running: 'Rolling out', done: 'Done', stopped: 'Stopped',
    denied: 'Denied', cancelled: 'Taken back',
  }
  return (
    <SectionCard title={`Rollout of version ${r.version}`} description={r.summary} flush
      badge={<Badge variant="light" radius="sm" tt="none"
        color={r.state === 'done' ? 'teal' : r.state === 'running' ? 'azure' : r.state === 'awaiting_approval' ? 'amber' : 'rose'}>
        {words[r.state]}
      </Badge>}
      action={admin ? (
        <Group gap={SP.snug}>
          {r.state === 'awaiting_approval' && mine && alone && (
            <ApproveOwn what="this rollout" loading={act.isPending} size="compact-sm"
              onApprove={(confirmSelf) => notifyRun(() => act.mutateAsync({ id: r.id, action: 'approve', confirmSelf }),
                { failure: 'Not approved', success: ['Approved', 'The first server gets it at its next check-in.'] })} />
          )}
          {r.state === 'awaiting_approval' && !mine && (
            <>
              <Button size="compact-sm" color="teal" leftSection={<IconCheck size={14} />} loading={act.isPending}
                onClick={() => go('approve', ['Approved', 'The first server gets it at its next check-in.'], 'Not approved')}>Approve</Button>
              <Button size="compact-sm" variant="default" color="rose" leftSection={<IconX size={14} />} loading={act.isPending}
                onClick={() => go('deny', ['Denied', 'No server is changed.'], 'Not denied')}>Deny</Button>
            </>
          )}
          {(r.state === 'running' || r.state === 'awaiting_approval') && (
            <Button size="compact-sm" variant="default" leftSection={<IconPlayerStop size={14} />} loading={act.isPending}
              onClick={() => go('stop', ['Stopped', 'Servers not reached yet keep what they hold.'], 'Not stopped')}>Stop</Button>
          )}
        </Group>
      ) : undefined}>
      <Box px="md" pb="sm" data-testid="policy-rollout">
        <Group gap="sm" mb="xs">
          <Progress value={r.total ? (100 * r.done) / r.total : 0} color={r.failed ? 'rose' : 'teal'} style={{ flex: 1 }} />
          <Text size={FS.meta} c="dimmed">{r.done} of {r.total} done{r.failed ? `, ${r.failed} failed` : ''}</Text>
        </Group>
        {r.message && r.state !== 'running' && <Text size={FS.meta} c="dimmed" mb="xs">{r.message}</Text>}
        <Table verticalSpacing={4}>
          <Table.Tbody>
            {(r.servers ?? []).map((c) => (
              <Table.Tr key={c.id}>
                <Table.Td w={70}><Text size={FS.micro} c="dimmed">{c.wave === 0 ? 'First' : `Group ${c.wave}`}</Text></Table.Td>
                <Table.Td>
                  <AnchorLink to="/assets/$assetId" params={{ assetId: c.assetId }} search={{ tab: 'firewall' }} size="sm" ff="monospace">
                    {c.hostname}
                  </AnchorLink>
                </Table.Td>
                <Table.Td><Text size={FS.meta} c="dimmed">{c.summary}</Text></Table.Td>
                <Table.Td ta="right">
                  <Badge size="sm" variant="light" radius="sm" tt="none" color={changeStateColor(c.state)} title={c.message}>
                    {changeStateWords(c.state)}
                  </Badge>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </Box>
    </SectionCard>
  )
}

function LinesCard({ p, admin }: { p: FirewallPolicy; admin: boolean }) {
  const [lines, setLines] = useState<FWLine[]>(p.lines)
  const [defaultOut, setDefaultOut] = useState(p.defaultOut)
  const [editing, setEditing] = useState<{ i: number; line: FWLine } | null>(null)
  const [note, setNote] = useState('')
  const save = useSaveFirewallPolicy(p.id)
  useEffect(() => { setLines(p.lines); setDefaultOut(p.defaultOut) }, [p.version, p.lines, p.defaultOut])
  const changed = JSON.stringify(lines) !== JSON.stringify(p.lines) || defaultOut !== p.defaultOut
  return (
    <SectionCard title="Lines" description="Every server with this policy holds these lines, beside its own." flush
      action={admin ? (
        <Button size="compact-sm" variant="default" leftSection={<IconPlus size={14} />}
          onClick={() => setEditing({ i: -1, line: emptyLine() })} data-testid="policy-add-line">
          Add a line
        </Button>
      ) : undefined}>
      <Table verticalSpacing="sm" horizontalSpacing="md" data-testid="policy-lines">
        <Table.Tbody>
          {lines.length === 0 && (
            <Table.Tr><Table.Td colSpan={3}><Text size={FS.body} c="dimmed">No lines.</Text></Table.Td></Table.Tr>
          )}
          {lines.map((l, i) => (
            <Table.Tr key={l.key ?? `new-${i}`}>
              <Table.Td><Text size="sm" fw={600}>{lineWords(l)}</Text></Table.Td>
              <Table.Td><Text size={FS.meta} c="dimmed">{l.note || '—'}</Text></Table.Td>
              <Table.Td ta="right">
                {admin && (
                  <Group gap={SP.tight} justify="flex-end" wrap="nowrap">
                    <Button size="compact-xs" variant="subtle" leftSection={<IconPencil size={12} />}
                      onClick={() => setEditing({ i, line: l })}>Change</Button>
                    <Button size="compact-xs" variant="subtle" color="rose" leftSection={<IconTrash size={12} />}
                      onClick={() => setLines(lines.filter((_, j) => j !== i))}>Remove</Button>
                  </Group>
                )}
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      <Stack gap="sm" p="md">
        <Group gap="sm" align="center">
          <Text size={FS.body}>Going out</Text>
          <SegmentedControl size="xs" disabled={!admin} value={defaultOut} onChange={(v) => setDefaultOut(v as 'allow' | 'deny')}
            data={[{ value: 'allow', label: 'Allow everything' }, { value: 'deny', label: 'Only the lines' }]} />
        </Group>
        {defaultOut === 'deny' && (
          <Text size={FS.meta} c="dimmed">
            Argus always lets out its own connection, name lookups and system updates. A server blocks going out only when
            every policy on it says so.
          </Text>
        )}
        {admin && changed && (
          <Group align="flex-end" gap="sm">
            <Textarea style={{ flex: 1 }} label="What changed" autosize minRows={1} value={note}
              onChange={(e) => setNote(e.currentTarget.value)} />
            <Button variant="default" onClick={() => { setLines(p.lines); setDefaultOut(p.defaultOut) }}>Undo</Button>
            <Button loading={save.isPending} data-testid="policy-save"
              onClick={() => void notifyRun(() => save.mutateAsync({ name: p.name, description: p.description, lines, defaultOut, note }),
                { failure: 'Not saved', success: ['Saved', 'The new version rolls out to the policy’s servers.'] }).then((ok) => { if (ok) setNote('') })}>
              Save and roll out
            </Button>
          </Group>
        )}
      </Stack>
      {editing && (
        <Modal opened onClose={() => setEditing(null)} title={<Text fw={700}>{editing.i < 0 ? 'Add a line' : 'Change a line'}</Text>}>
          <LineEditor initial={editing.line} policyId={p.id} onCancel={() => setEditing(null)}
            onDone={(l) => {
              setLines(editing.i < 0 ? [...lines, l] : lines.map((x, j) => (j === editing.i ? { ...l, key: x.key } : x)))
              setEditing(null)
            }} />
        </Modal>
      )}
    </SectionCard>
  )
}

function LineEditor({ initial, policyId, onCancel, onDone }: {
  initial: FWLine
  policyId: string
  onCancel: () => void
  onDone: (l: FWLine) => void
}) {
  const [line, setLine] = useState(initial)
  return (
    <Stack gap="md">
      <LineForm value={line} onChange={setLine} policyId={policyId} />
      <Text size={FS.meta} c="dimmed">On servers with ufw, a Ping line is left to ufw, which lets ping in by itself.</Text>
      <Group justify="flex-end">
        <Button variant="default" onClick={onCancel}>Cancel</Button>
        <Button onClick={() => onDone(line)}>Done</Button>
      </Group>
    </Stack>
  )
}

function TargetsCard({ p, admin }: { p: FirewallPolicy; admin: boolean }) {
  const [kind, setKind] = useState<'server' | 'tag'>('tag')
  const [value, setValue] = useState<string | null>(null)
  const { data: assets } = useQuery({ ...assetsQuery(), enabled: admin && kind === 'server' })
  const target = useFirewallTarget(p.id)
  return (
    <SectionCard title="Where it is" description="Servers by name, or every server with a tag, now and later." flush>
      <Table verticalSpacing="sm" horizontalSpacing="md" data-testid="policy-targets">
        <Table.Tbody>
          {p.targets.length === 0 && (
            <Table.Tr><Table.Td colSpan={2}><Text size={FS.body} c="dimmed">Nowhere yet.</Text></Table.Td></Table.Tr>
          )}
          {p.targets.map((t) => (
            <Table.Tr key={t.id}>
              <Table.Td>
                {t.tag ? <Text size="sm">Every server tagged <b>{t.tag}</b></Text> : (
                  <AnchorLink to="/assets/$assetId" params={{ assetId: t.assetId! }} search={{ tab: 'firewall' }} size="sm" ff="monospace">
                    {t.hostname}
                  </AnchorLink>
                )}
              </Table.Td>
              <Table.Td ta="right">
                {admin && (
                  <Button size="compact-xs" variant="subtle" color="rose" loading={target.isPending}
                    onClick={() => void notifyRun(() => target.mutateAsync({ remove: t.id }),
                      { failure: 'Not taken off', success: ['Taken off', 'The servers drop its lines, one first, then five at a time.'] })}>
                    Take off
                  </Button>
                )}
              </Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      {admin && (
        <Group p="md" gap="sm" align="flex-end">
          <SegmentedControl value={kind} onChange={(v) => { setKind(v as 'server' | 'tag'); setValue(null) }}
            data={[{ value: 'tag', label: 'A tag' }, { value: 'server', label: 'A server' }]} />
          {kind === 'tag' ? (
            <TextInput style={{ flex: 1 }} placeholder="web" value={value ?? ''} onChange={(e) => setValue(e.currentTarget.value)}
              aria-label="Tag" />
          ) : (
            <Select style={{ flex: 1 }} searchable placeholder="Pick a server" value={value} onChange={setValue} aria-label="Server"
              data={(assets ?? []).filter((a) => a.kind === 'server' || a.kind === 'workstation').map((a) => ({ value: a.id, label: a.hostname }))} />
          )}
          <Button disabled={!value?.trim()} loading={target.isPending} data-testid="policy-put-on"
            onClick={() => void notifyRun(() => target.mutateAsync({ add: kind === 'tag' ? { tag: value!.trim() } : { assetId: value! } }),
              { failure: 'Not put on', success: ['Put on', 'It rolls out: one server first, then five at a time.'] }).then((ok) => { if (ok) setValue(null) })}>
            Put on
          </Button>
        </Group>
      )}
    </SectionCard>
  )
}

function DeleteCard({ p }: { p: FirewallPolicy }) {
  const del = useDeleteFirewallPolicy()
  const navigate = useNavigate()
  const inUse = p.targets.length > 0 || p.servers > 0
  return (
    <SectionCard title="Delete this policy" danger
      description={inUse ? 'Take it off its servers and tags first.' : 'No server holds it. Its versions go with it.'}
      action={
        <Button color="rose" variant="light" disabled={inUse} loading={del.isPending}
          onClick={() => void notifyRun(() => del.mutateAsync(p.id), { failure: 'Not deleted', success: ['Deleted', p.name] })
            .then((ok) => { if (ok) void navigate({ to: '/assets/firewall-policies' }) })}>
          Delete
        </Button>
      }>
      <Text size={FS.meta} c="dimmed" px="md" pb="md">Lines for “servers with {p.name}” in other policies must change first.</Text>
    </SectionCard>
  )
}

