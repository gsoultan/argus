import { useEffect, useState } from 'react'
import {
  Alert, Anchor, Badge, Box, Button, Code, CopyButton, Group, Modal, NumberInput, SegmentedControl, Select, Stack, Switch, Table,
  Text, TextInput, Textarea,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconAlertTriangle, IconCopy, IconEye, IconLock } from '@tabler/icons-react'
import { DataTable, EmptyState, FS, INK, Mono, SP, Toolbar, relTime, run } from '@argus/shell'
import {
  NEEDS_APPROVAL, reveal, secretsQuery, useAskToRead, useDeleteSecret, usePutSecret, useRestoreSecret,
  type Revealed, type Secret,
} from './api'

/** How long a revealed value stays on screen. */
const SHOWN_FOR_MS = 60_000

export function SecretsTab({ creating, onCreated, openSettings }: {
  creating: boolean
  onCreated: () => void
  openSettings: () => void
}) {
  const [prefix, setPrefix] = useState('')
  const [deleted, setDeleted] = useState(false)
  const typed = prefix.trim()
  const { data, isLoading, error } = useQuery(secretsQuery(typed, deleted))
  const [open, setOpen] = useState<Secret | null>(null)
  const [editing, setEditing] = useState<Secret | null>(null)
  const restore = useRestoreSecret()

  return (
    <Stack gap="md">
      <Toolbar
        right={<Switch label="Deleted, still recoverable" checked={deleted} onChange={(e) => setDeleted(e.currentTarget.checked)} />}
      >
        <TextInput
          aria-label="Folder"
          placeholder="Folder, such as prod/"
          value={prefix}
          onChange={(e) => setPrefix(e.currentTarget.value)}
          w={240}
        />
      </Toolbar>

      {error && <Alert color="rose"><Text size={FS.body}>Could not read the secrets: {error.message}</Text></Alert>}
      {data?.unreachable.map((u) => (
        <Alert key={u.server} color="amber" icon={<IconAlertTriangle size={16} />} title={`${u.server} did not answer`} p="xs">
          <Text size={FS.body}>
            Its secrets are not listed. {u.error}{' '}
            <Anchor component="button" size={FS.body} onClick={openSettings}>Settings › Secrets servers</Anchor>
          </Text>
        </Alert>
      ))}

        <DataTable
          columns={['Name', 'Type', 'Version', 'Server', 'Changed', '']}
          minWidth={760}
          loading={isLoading}
          isEmpty={!!data && data.secrets.length === 0}
          empty={
            <EmptyState
              compact
              icon={IconLock}
              title={deleted ? 'Nothing deleted' : typed ? `No secrets under ${typed}` : 'No secrets you may see'}
              description={deleted ? 'Deleted secrets are listed here until they are purged.' : 'A rule decides who sees which names. Ask an admin, or add one if you may write here.'}
            />
          }
        >
          {data?.secrets.map((s) => (
            <Table.Tr key={s.name}>
              <Table.Td><Mono>{s.name}</Mono></Table.Td>
              <Table.Td><Text size={FS.meta}>{s.type === 'kv' ? 'Fields' : s.type === 'file' ? 'File' : 'Text'}</Text></Table.Td>
              <Table.Td><Text size={FS.meta}>{s.labels.current ?? '—'}</Text></Table.Td>
              <Table.Td><Text size={FS.meta}>{s.server}</Text></Table.Td>
              <Table.Td>
                <Text size={FS.meta} c={s.deletedAt ? INK.denied : 'dimmed'}>
                  {s.deletedAt ? `Deleted ${relTime(s.deletedAt)}, purged ${relTime(s.purgeAt ?? null)}` : relTime(s.updatedAt)}
                </Text>
              </Table.Td>
              <Table.Td>
                <Group gap={SP.snug} justify="flex-end" wrap="nowrap">
                  {s.deletedAt ? (
                    <Button size="xs" variant="default" loading={restore.isPending && restore.variables?.name === s.name}
                      onClick={() => void run(() => restore.mutateAsync({ name: s.name }), {
                        failure: 'Not restored', success: ['Restored', `${s.name} is back, with every version.`],
                      })}>
                      Restore
                    </Button>
                  ) : (
                    <Button size="xs" variant="default" onClick={() => setOpen(s)}>Open</Button>
                  )}
                </Group>
              </Table.Td>
            </Table.Tr>
          ))}
        </DataTable>

      {open && (
        <SecretDialog secret={open} onClose={() => setOpen(null)} onEdit={() => { setEditing(open); setOpen(null) }} />
      )}
      {(creating || editing) && (
        <SecretForm secret={editing} onClose={() => { setEditing(null); onCreated() }} />
      )}
    </Stack>
  )
}

/* ── One secret ────────────────────────────────────────────────────────── */

function SecretDialog({ secret, onClose, onEdit }: { secret: Secret; onClose: () => void; onEdit: () => void }) {
  const current = secret.versions.find((v) => v.n === secret.labels.current)
  const fields = current?.fields ?? []
  const [field, setField] = useState<string>(fields[0] ?? '')
  const [reason, setReason] = useState('')
  const [shown, setShown] = useState<Revealed | null>(null)
  const [reading, setReading] = useState(false)
  const [needsApproval, setNeedsApproval] = useState(false)
  const [deleting, setDeleting] = useState(false)

  // A value on screen is cleared after a minute, and when the dialog closes.
  useEffect(() => {
    if (!shown) return
    const t = setTimeout(() => setShown(null), SHOWN_FOR_MS)
    return () => clearTimeout(t)
  }, [shown])

  const read = () => {
    setReading(true)
    setNeedsApproval(false)
    void reveal(secret.name, secret.type === 'kv' ? field : '', reason.trim())
      .then(setShown)
      .catch((err: unknown) => {
        if (err instanceof Error && err.message.includes(NEEDS_APPROVAL)) setNeedsApproval(true)
        else void run(() => Promise.reject(err), { failure: 'Not opened' })
      })
      .finally(() => setReading(false))
  }

  if (deleting) return <DeleteSecret secret={secret} onClose={() => setDeleting(false)} onDeleted={onClose} />

  return (
    <Modal opened onClose={onClose} title={<Mono>{secret.name}</Mono>} size="lg">
      <Stack gap="sm">
        {secret.description && <Text size={FS.body}>{secret.description}</Text>}
        <Text size={FS.meta} c="dimmed">
          On {secret.server}. Version {secret.labels.current ?? '—'} of {secret.versions.length}, changed {relTime(secret.updatedAt)}
          {current ? ` by ${current.createdBy}` : ''}{current?.generated ? ', generated by argus-secret' : ''}.
        </Text>

        {shown ? (
          <Box>
            <Group justify="space-between" mb={SP.tight}>
              <Text size={FS.meta} c="dimmed">
                {shown.field ? `${shown.field}, ` : ''}version {shown.version}. Hidden again in a minute.
              </Text>
              <CopyButton value={shown.value} timeout={1500}>
                {({ copied, copy }) => (
                  <Button size="xs" variant="default" leftSection={<IconCopy size={14} />} onClick={copy}>
                    {copied ? 'Copied' : 'Copy'}
                  </Button>
                )}
              </CopyButton>
            </Group>
            <Code block style={{ overflowWrap: 'anywhere', whiteSpace: 'pre-wrap' }}>{shown.value}</Code>
          </Box>
        ) : needsApproval ? (
          <AskToRead name={secret.name} reason={reason} onDone={onClose} />
        ) : (
          <Stack gap="xs">
            {secret.type === 'kv' && fields.length > 0 && (
              <Select label="Field" allowDeselect={false} value={field} onChange={(v) => setField(v ?? '')} data={fields} />
            )}
            <TextInput
              label="Why do you need it?"
              description="Kept with the read in the audit log."
              value={reason}
              onChange={(e) => setReason(e.currentTarget.value)}
              maxLength={500}
            />
            <Group justify="flex-end">
              <Button leftSection={<IconEye size={14} />} disabled={!reason.trim()} loading={reading} onClick={read}>
                Show value
              </Button>
            </Group>
          </Stack>
        )}

        {secret.versions.length > 1 && (
          <Box>
            <Text size={FS.meta} fw={600} mb={SP.tight}>Versions</Text>
            <Stack gap={SP.hair}>
              {[...secret.versions].reverse().map((v) => (
                <Group key={v.n} gap={SP.snug}>
                  <Text size={FS.meta}>{v.n}</Text>
                  {Object.entries(secret.labels).filter(([, n]) => n === v.n).map(([l]) => (
                    <Badge key={l} size="xs" variant="light" color="slate">{l}</Badge>
                  ))}
                  <Text size={FS.meta} c="dimmed">{relTime(v.createdAt)} by {v.createdBy}</Text>
                </Group>
              ))}
            </Stack>
          </Box>
        )}

        <Group justify="space-between">
          <Button variant="subtle" color="rose" onClick={() => setDeleting(true)}>Delete</Button>
          <Group gap="xs">
            <Button variant="default" onClick={onClose}>Close</Button>
            <Button variant="default" onClick={onEdit}>New version</Button>
          </Group>
        </Group>
      </Stack>
    </Modal>
  )
}

function AskToRead({ name, reason, onDone }: { name: string; reason: string; onDone: () => void }) {
  const ask = useAskToRead()
  const [why, setWhy] = useState(reason)
  const [minutes, setMinutes] = useState('60')
  return (
    <Stack gap="xs">
      <Alert color="amber" icon={<IconLock size={16} />} p="xs" title="This secret needs an approval">
        <Text size={FS.body}>Ask, and an approver is told. Once approved you can open it for the time you ask for.</Text>
      </Alert>
      <TextInput label="Why do you need it?" value={why} onChange={(e) => setWhy(e.currentTarget.value)} maxLength={500} />
      <Select label="For" allowDeselect={false} value={minutes} onChange={(v) => setMinutes(v ?? '60')}
        data={[{ value: '15', label: '15 minutes' }, { value: '60', label: '1 hour' }, { value: '240', label: '4 hours' }, { value: '480', label: '8 hours' }]} />
      <Group justify="flex-end">
        <Button disabled={!why.trim()} loading={ask.isPending}
          onClick={() => void run(() => ask.mutateAsync({ name, reason: why.trim(), minutes: Number(minutes) }), {
            failure: 'Not asked', success: ['Asked', 'You will find it under Requests once it is decided.'],
          }).then((ok) => { if (ok) onDone() })}>
          Ask for approval
        </Button>
      </Group>
    </Stack>
  )
}

function DeleteSecret({ secret, onClose, onDeleted }: { secret: Secret; onClose: () => void; onDeleted: () => void }) {
  const del = useDeleteSecret()
  const [reason, setReason] = useState('')
  return (
    <Modal opened onClose={onClose} title={`Delete ${secret.name}?`}>
      <Stack gap="sm">
        <Text size={FS.body}>
          Nothing can read it from now on. It can be restored, with every version, until it is purged a few days from now.
        </Text>
        <TextInput label="Why?" value={reason} onChange={(e) => setReason(e.currentTarget.value)} maxLength={500} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" disabled={!reason.trim()} loading={del.isPending}
            onClick={() => void run(() => del.mutateAsync({ name: secret.name, reason: reason.trim() }), {
              failure: 'Not deleted', success: ['Deleted', `${secret.name} can be restored until it is purged.`],
            }).then((ok) => { if (ok) { onClose(); onDeleted() } })}>
            Delete secret
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/* ── New secret, or a new version ──────────────────────────────────────── */

function SecretForm({ secret, onClose }: { secret: Secret | null; onClose: () => void }) {
  const put = usePutSecret()
  const [name, setName] = useState(secret?.name ?? '')
  const [type, setType] = useState<'text' | 'kv'>(secret?.type === 'kv' ? 'kv' : 'text')
  const [description, setDescription] = useState(secret?.description ?? '')
  const [how, setHow] = useState<'type' | 'generate'>('type')
  const [value, setValue] = useState('')
  const [length, setLength] = useState<number | string>(32)
  const [reason, setReason] = useState('')

  // Fields are written as one per line, name=value.
  const kv = () => {
    const out: Record<string, string> = {}
    for (const line of value.split('\n')) {
      const i = line.indexOf('=')
      if (i > 0) out[line.slice(0, i).trim()] = line.slice(i + 1)
    }
    return out
  }
  const generating = how === 'generate' && type === 'text'
  const valueOK = generating || (type === 'kv' ? Object.keys(kv()).length > 0 : value.length > 0)
  const save = () => void run(async () => {
    await put.mutateAsync({
      name: name.trim(), type, description: description.trim(), reason: reason.trim(),
      value: generating ? null : type === 'kv' ? JSON.stringify(kv()) : value,
      generateLength: Number(length) || 32,
    })
    setValue('')
  }, {
    failure: 'Not saved',
    success: ['Saved', secret ? `${name.trim()} has a new version.` : `${name.trim()} is saved.`],
  }).then((ok) => { if (ok) onClose() })

  return (
    <Modal opened onClose={onClose} title={secret ? `New version of ${secret.name}` : 'New secret'} size="lg">
      <Stack gap="sm">
        {!secret && (
          <TextInput label="Name" description="Folder first, such as prod/payments/db. The folder decides which server keeps it."
            value={name} onChange={(e) => setName(e.currentTarget.value)} data-autofocus />
        )}
        {!secret && (
          <SegmentedControl value={type} onChange={(v) => setType(v as 'text' | 'kv')}
            data={[{ value: 'text', label: 'One value' }, { value: 'kv', label: 'Fields' }]} />
        )}
        <TextInput label="Description" value={description} onChange={(e) => setDescription(e.currentTarget.value)} />
        {type === 'text' && (
          <SegmentedControl value={how} onChange={(v) => setHow(v as 'type' | 'generate')}
            data={[{ value: 'type', label: 'Type it' }, { value: 'generate', label: 'Generate it' }]} />
        )}
        {how === 'generate' && type === 'text' ? (
          <NumberInput label="Length" min={16} max={128} value={length} onChange={setLength}
            description="argus-secret makes it. Nobody sees it until it is read." />
        ) : (
          <Textarea
            label={type === 'kv' ? 'Fields, one per line as name=value' : 'Value'}
            description="Sealed in this browser to the secrets server's key before it is sent."
            autosize minRows={3} maxRows={10} ff="monospace" value={value} onChange={(e) => setValue(e.currentTarget.value)}
          />
        )}
        <TextInput label="Why?" value={reason} onChange={(e) => setReason(e.currentTarget.value)} maxLength={500} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!name.trim() || !valueOK} loading={put.isPending} onClick={save}>Save</Button>
        </Group>
      </Stack>
    </Modal>
  )
}
