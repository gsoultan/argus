import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Code, Group, Modal, ScrollArea, Stack, Table, Tabs, Text, Textarea, UnstyledButton,
} from '@mantine/core'
import { notifications } from '@mantine/notifications'
import { IconAlertTriangle, IconCheck, IconDatabase, IconShieldCheck, IconX } from '@tabler/icons-react'
import { relTime } from '~/components/primitives'
import { EnvBadge, ImpactText, ImpactTiles, PreviewRows } from '~/components/ChangeImpact'
import { BIG_CHANGE, impactOf, impactWords, shortName } from '~/lib/changeApproval'
import { notifyError } from '~/lib/notify'
import { useDecideRequest } from '~/lib/queries'
import { FS } from '~/theme'
import type { AccessRequest } from '~/types/domain'

/*
Change requests on Approvals: a table of what waits, with how many rows each
changes so the big ones stand out, and the review of one, with the counts per
table, the first affected rows and the statements, from the dry run when it
was asked for.
*/

const MIN_DENIAL_NOTE = 5

/** Pending change requests, biggest impact first when asked. */
export function ChangesTable({ reqs, mine, onOpen }: { reqs: AccessRequest[]; mine?: string; onOpen: (r: AccessRequest) => void }) {
  const [byImpact, setByImpact] = useState(false)
  if (!reqs.length) return null
  const rows = [...reqs].sort((a, b) => byImpact
    ? (impactOf(b.statement?.summary).rows ?? -1) - (impactOf(a.statement?.summary).rows ?? -1)
    : Date.parse(b.createdAt) - Date.parse(a.createdAt))
  const forMe = reqs.filter((r) => r.requesterEmail.toLowerCase() !== mine?.toLowerCase()).length
  return (
    <Box className="argus-changes" mb="md">
      <Group justify="space-between" mb={6}>
        <Group gap={6}>
          <IconShieldCheck size={16} />
          <Text fw={600} size="sm">Changes waiting</Text>
        </Group>
        {forMe > 0 && <Badge color="amber" variant="light" radius="sm" tt="none">{forMe} waiting for you</Badge>}
      </Group>
      <ScrollArea type="auto">
        <Table highlightOnHover verticalSpacing="xs" miw={620} aria-label="Changes waiting">
          <Table.Thead>
            <Table.Tr>
              <Table.Th>Who</Table.Th><Table.Th>Database</Table.Th><Table.Th>Environment</Table.Th>
              <Table.Th>
                <UnstyledButton onClick={() => setByImpact((v) => !v)} aria-label="Sort by impact">
                  <Text size={FS.meta} fw={700}>Impact{byImpact ? ' ↓' : ''}</Text>
                </UnstyledButton>
              </Table.Th>
              <Table.Th>Asked</Table.Th>
            </Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {rows.map((r) => (
              <Table.Tr key={r.id} onClick={() => onOpen(r)} style={{ cursor: 'pointer' }}>
                <Table.Td><Text size={FS.body}>{shortName(r.requesterEmail)}</Text></Table.Td>
                <Table.Td><Text size={FS.body} ff="monospace">{r.statement?.databaseName}</Text></Table.Td>
                <Table.Td>{r.statement?.databaseLabel && <EnvBadge label={r.statement.databaseLabel} />}</Table.Td>
                <Table.Td><ImpactText impact={impactOf(r.statement?.summary)} fallback={`${r.statement?.items.length ?? 0} statements`} /></Table.Td>
                <Table.Td><Text size={FS.body} c="dimmed">{relTime(r.createdAt)}</Text></Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </ScrollArea>
      <Text size={FS.micro} c="dimmed" mt={4}>Over {BIG_CHANGE.toLocaleString()} rows shows in red. Press Impact to see the biggest first.</Text>
    </Box>
  )
}

/** One change request, opened: what it changes, and the decision. */
export function ChangeReview({ request: r, canDecide, onClose }: { request: AccessRequest | null; canDecide: boolean; onClose: () => void }) {
  const decide = useDecideRequest()
  const [note, setNote] = useState('')
  const [tab, setTab] = useState<string | null>('rows')
  if (!r?.statement) return <Modal opened={false} onClose={onClose}>{null}</Modal>
  const st = r.statement
  const impact = impactOf(st.summary)
  const words = impactWords(impact)
  const n = st.items.length
  const who = shortName(r.requesterEmail)
  const preview = impact.preview ?? []
  const hidden = !preview.length && (impact.rows ?? 0) > 0 && impact.checked
  const pending = r.state === 'pending'

  const onDecide = async (decision: 'approved' | 'denied') => {
    if (decision === 'denied' && note.trim().length < MIN_DENIAL_NOTE) {
      notifyError('Reason required', new Error('a denial must say why, so the asker can act on it'))
      return
    }
    try {
      await decide.mutateAsync({ id: r.id, decision, note })
    } catch (err) {
      notifyError(decision === 'approved' ? 'Not approved' : 'Not denied', err)
      return
    }
    notifications.show({
      color: decision === 'approved' ? 'teal' : 'rose',
      title: decision === 'approved' ? 'Approved' : 'Denied',
      message: decision === 'approved'
        ? `${who} can run ${n === 1 ? 'it' : 'them'} once, within the hour.`
        : `${who} was told why.`,
    })
    onClose()
  }

  return (
    <Modal opened onClose={onClose} size="xl" centered
      title={
        <Group gap="xs" wrap="nowrap">
          <IconDatabase size={18} />
          <Text fw={650}>{pending && canDecide ? `Approve ${who}'s change on ${st.databaseName}?` : `${who}'s change on ${st.databaseName}`}</Text>
          {st.databaseLabel && <EnvBadge label={st.databaseLabel} />}
        </Group>
      }>
      <Stack gap="sm">
        <Group justify="space-between" align="flex-start" wrap="nowrap">
          <Text size={FS.meta} c="dimmed">
            {n === 1 ? '1 statement' : `${n} statements`} · asked {relTime(r.createdAt)}
            {r.asEditor ? ` · you can approve as an editor of ${st.databaseName}` : ''}
          </Text>
          {words && (
            <Badge color={(impact.rows ?? 0) > BIG_CHANGE ? 'rose' : 'amber'} variant="light" radius="sm" tt="none"
              leftSection={<IconAlertTriangle size={12} />}>
              {words.split(' · ')[0]} affected
            </Badge>
          )}
        </Group>
        {r.asEditor && pending && (
          <Text size={FS.meta} c="dimmed">You're deciding as an editor of {st.databaseName}. Your name goes in the audit log as the approver.</Text>
        )}
        {impact.checked && (
          <Stack gap={4}>
            <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600}>Impact, from a dry run when {who} asked</Text>
            <ImpactTiles impact={impact} />
          </Stack>
        )}
        <Tabs value={tab} onChange={setTab}>
          <Tabs.List>
            <Tabs.Tab value="rows">Affected rows{preview.length ? ` (first ${preview.reduce((k, p) => k + p.rows.length, 0)})` : ''}</Tabs.Tab>
            <Tabs.Tab value="sql">Statements</Tabs.Tab>
          </Tabs.List>
          <Tabs.Panel value="rows" pt="xs">
            {preview.length ? <PreviewRows preview={preview} />
              : hidden ? <Text size={FS.meta} c="dimmed">Only people who can read {st.databaseName} see the affected rows; you see the counts.</Text>
              : <Text size={FS.meta} c="dimmed">No rows were previewed for this change{impact.checked ? '' : ': it was asked for without a dry run'}.</Text>}
          </Tabs.Panel>
          <Tabs.Panel value="sql" pt="xs">
            <ScrollArea.Autosize mah={260} type="auto">
              <Stack gap={6}>
                {st.items.map((it, i) => (
                  <Code key={i} block style={{ whiteSpace: 'pre-wrap', fontSize: 12, opacity: it.used ? 0.6 : 1 }}>
                    {it.sql}
                    {it.params?.length ? `\n-- values: ${it.params.map((p) => (p === null ? 'NULL' : JSON.stringify(p))).join(', ')}` : ''}
                    {it.rows !== undefined ? `\n-- ${it.rows.toLocaleString()} ${it.rows === 1 ? 'row' : 'rows'} at most` : ''}
                    {it.used ? '\n-- ran' : ''}
                  </Code>
                ))}
              </Stack>
            </ScrollArea.Autosize>
          </Tabs.Panel>
        </Tabs>
        <Alert variant="light" color="slate" p="xs">
          <Text size={FS.meta}>
            Approved, {n === 1 ? 'it runs' : 'each runs'} once{words ? ` and stops if it would change more than these ${words.split(' · ')[0]} (someone else may have changed ${st.databaseName} since)` : ''}. Deleted rows go to the trash.
          </Text>
        </Alert>
        <Text size={FS.body} fs="italic">“{r.justification}”</Text>
        {pending && canDecide ? (
          <>
            <Textarea label="Note (needed to deny)" placeholder="Optional when approving" autosize minRows={2}
              value={note} onChange={(e) => setNote(e.currentTarget.value)} />
            <Group justify="flex-end">
              <Button variant="default" leftSection={<IconX size={14} />} loading={decide.isPending} onClick={() => void onDecide('denied')}>Deny</Button>
              <Button leftSection={<IconCheck size={14} />} loading={decide.isPending} onClick={() => void onDecide('approved')}>
                Approve {n === 1 ? '' : `${n} statements`}{words ? `${n === 1 ? '' : ', '}${words.split(' · ')[0]}` : n === 1 ? 'to run once' : ''}
              </Button>
            </Group>
          </>
        ) : pending ? (
          <Text size={FS.meta} c="dimmed">
            {r.requesterEmail.length && !canDecide ? 'Waiting for someone else to decide. You never decide your own.' : ''}
          </Text>
        ) : (
          <Text size={FS.meta} c="dimmed">{r.state} by {r.decidedByEmail}{r.decisionNote ? `: ${r.decisionNote}` : ''}</Text>
        )}
      </Stack>
    </Modal>
  )
}
