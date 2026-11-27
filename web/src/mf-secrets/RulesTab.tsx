import { useState } from 'react'
import { Alert, Badge, Button, Checkbox, Group, Modal, Select, Stack, Switch, Table, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconPlus, IconShieldLock, IconX } from '@tabler/icons-react'
import { DataTable, EmptyState, FS, Mono, SP, run } from '@argus/shell'
import { CAN_LABEL, SUBJECT_LABEL, rulesQuery, useRemoveRule, useSaveRule, type SecretRule, type Subject } from './api'

export function RulesTab({ adding, onAdded }: { adding: boolean; onAdded: () => void }) {
  const { data, isLoading, error } = useQuery(rulesQuery())
  const [editing, setEditing] = useState<SecretRule | null>(null)
  const [removing, setRemoving] = useState<SecretRule | null>(null)
  return (
    <Stack gap="md">
      {error && <Alert color="rose"><Text size={FS.body}>Could not read the rules: {error.message}</Text></Alert>}
      <DataTable
        columns={['Secrets', 'Who', 'May', 'Approval', '']}
        minWidth={820}
        loading={isLoading}
        isEmpty={!!data && data.length === 0}
        empty={<EmptyState compact icon={IconShieldLock} title="No rules yet" description="Until there is one, nobody but argus-control can do anything with a secret." />}
      >
        {data?.map((r) => (
          <Table.Tr key={r.id}>
            <Table.Td>
              <Mono>{r.path || 'Every secret'}</Mono>
              {r.note && <Text size={FS.micro} c="dimmed">{r.note}</Text>}
            </Table.Td>
            <Table.Td>
              <Group gap={SP.tight}>
                {r.subjects.map((s) => <Badge key={`${s.kind}:${s.id}`} size="sm" variant="default" tt="none">{SUBJECT_LABEL[s.kind] ?? s.kind}: {s.id}</Badge>)}
              </Group>
            </Table.Td>
            <Table.Td><Text size={FS.meta}>{r.can.map((c) => CAN_LABEL[c] ?? c).join(', ')}</Text></Table.Td>
            <Table.Td>{r.needsApproval ? <Badge size="sm" variant="light" color="amber">Needs one</Badge> : <Text size={FS.meta} c="dimmed">No</Text>}</Table.Td>
            <Table.Td>
              <Group gap={SP.snug} justify="flex-end" wrap="nowrap">
                <Button size="xs" variant="default" onClick={() => setEditing(r)}>Edit</Button>
                <Button size="xs" variant="subtle" color="rose" onClick={() => setRemoving(r)}>Remove</Button>
              </Group>
            </Table.Td>
          </Table.Tr>
        ))}
      </DataTable>
      {(adding || editing) && <RuleForm rule={editing} onClose={() => { setEditing(null); onAdded() }} />}
      {removing && <RemoveRule rule={removing} onClose={() => setRemoving(null)} />}
    </Stack>
  )
}

const KINDS = Object.entries(SUBJECT_LABEL).map(([value, label]) => ({ value, label }))
const ID_HINT: Record<string, string> = {
  server: 'The server’s id, from its page', group: 'A server group or tag', app: 'The app token’s id',
  person: 'Their email', team: 'The team’s id', role: 'owner, admin, approver, operator or auditor',
}

function RuleForm({ rule, onClose }: { rule: SecretRule | null; onClose: () => void }) {
  const save = useSaveRule()
  const [path, setPath] = useState(rule?.path ?? '')
  const [subjects, setSubjects] = useState<Subject[]>(rule?.subjects ?? [])
  const [kind, setKind] = useState('team')
  const [id, setId] = useState('')
  const [can, setCan] = useState<string[]>(rule?.can ?? ['read'])
  const [needsApproval, setNeedsApproval] = useState(rule?.needsApproval ?? false)
  const [note, setNote] = useState(rule?.note ?? '')
  const add = () => {
    const s = { kind, id: id.trim() }
    if (s.id && !subjects.some((x) => x.kind === s.kind && x.id === s.id)) setSubjects([...subjects, s])
    setId('')
  }
  return (
    <Modal opened onClose={onClose} title={rule ? 'Change rule' : 'New rule'} size="lg">
      <Stack gap="sm">
        <TextInput label="Secrets" description="A folder ending in /, such as prod/payments/, one secret, or empty for every secret."
          value={path} onChange={(e) => setPath(e.currentTarget.value)} />
        <Stack gap={SP.tight}>
          <Text size={FS.body} fw={500}>Who</Text>
          <Group gap={SP.tight}>
            {subjects.map((s) => (
              <Badge key={`${s.kind}:${s.id}`} size="sm" variant="default" tt="none"
                rightSection={<IconX size={11} style={{ cursor: 'pointer' }} aria-label={`Remove ${s.id}`}
                  onClick={() => setSubjects(subjects.filter((x) => x !== s))} />}>
                {SUBJECT_LABEL[s.kind]}: {s.id}
              </Badge>
            ))}
          </Group>
          <Group gap="xs" align="flex-end" wrap="nowrap">
            <Select aria-label="Kind" allowDeselect={false} data={KINDS} value={kind} onChange={(v) => setKind(v ?? 'team')} w={150} />
            <TextInput aria-label="Who" placeholder={ID_HINT[kind]} value={id} onChange={(e) => setId(e.currentTarget.value)}
              onKeyDown={(e) => { if (e.key === 'Enter') { e.preventDefault(); add() } }} style={{ flex: 1 }} />
            <Button variant="default" leftSection={<IconPlus size={14} />} disabled={!id.trim()} onClick={add}>Add</Button>
          </Group>
        </Stack>
        <Checkbox.Group label="May" value={can} onChange={setCan}>
          <Group gap="md" mt={SP.tight}>
            {Object.entries(CAN_LABEL).map(([v, l]) => <Checkbox key={v} value={v} label={l} />)}
          </Group>
        </Checkbox.Group>
        <Switch label="Reading needs an approval first" checked={needsApproval} onChange={(e) => setNeedsApproval(e.currentTarget.checked)} />
        <TextInput label="Note" value={note} onChange={(e) => setNote(e.currentTarget.value)} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={subjects.length === 0 || can.length === 0} loading={save.isPending}
            onClick={() => void run(() => save.mutateAsync({ ...(rule ? { id: rule.id } : {}), rule: { path: path.trim(), subjects, can, needsApproval, note: note.trim() } }), {
              failure: 'Rule not saved', success: ['Saved', 'The servers that keep these secrets get it within a minute.'],
            }).then((ok) => { if (ok) onClose() })}>
            Save rule
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function RemoveRule({ rule, onClose }: { rule: SecretRule; onClose: () => void }) {
  const remove = useRemoveRule()
  return (
    <Modal opened onClose={onClose} title="Remove this rule?">
      <Stack gap="sm">
        <Text size={FS.body}>What it allowed on <Mono>{rule.path || 'every secret'}</Mono> stops within a minute, unless another rule allows it.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" loading={remove.isPending}
            onClick={() => void run(() => remove.mutateAsync(rule.id), { failure: 'Rule not removed', success: ['Removed', 'The servers get the change within a minute.'] })
              .then((ok) => { if (ok) onClose() })}>
            Remove rule
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
