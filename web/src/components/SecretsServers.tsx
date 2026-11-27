import { useState } from 'react'
import {
  Alert, Badge, Box, Button, Code, CopyButton, Group, Modal, SegmentedControl, Skeleton, Stack, Table, TagsInput, Text, TextInput,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconCopy, IconLockPassword, IconPlus } from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { Mono, relTime } from '~/components/primitives'
import { isConfigured } from '~/lib/live'
import { run } from '~/lib/notify'
import {
  enrolCommand, secretsServersQuery, useAddSecretsServer, useChangeSecretsServer, useNewEnrolToken, useRemoveSecretsServer,
  useResumeSecretsLog, useSyncSecretsServer, type Enrolment, type SecretsServer, type SecretsServerInput, type SecretsServersView,
} from '~/lib/secretsServers'
import { FS, INK, SP } from '~/theme'

/**
 * Settings › Secrets servers: the organization's argus-secret servers, which
 * folders each keeps, and whether argus-control is keeping them in step.
 * Admins and owners only; the server says the same.
 */
export function SecretsServers() {
  const { data: v, isLoading, error } = useQuery({ ...secretsServersQuery(), enabled: isConfigured() })
  const [editing, setEditing] = useState<SecretsServer | 'new' | null>(null)
  const [removing, setRemoving] = useState<SecretsServer | null>(null)
  const [resuming, setResuming] = useState<SecretsServer | null>(null)
  const [enrolment, setEnrolment] = useState<Enrolment | null>(null)
  const sync = useSyncSecretsServer()
  const token = useNewEnrolToken()

  if (!isConfigured()) {
    return (
      <Alert color="slate">
        <Text size={FS.body}>Secrets servers are added on a control plane. Connect this console to Argus to add them.</Text>
      </Alert>
    )
  }
  if (error) return <Alert color="rose"><Text size={FS.body}>{error.message}</Text></Alert>
  if (isLoading || !v) return <Skeleton h={320} />

  return (
    <Stack gap="sm">
      <SectionCard
        title="Secrets servers"
        icon={IconLockPassword}
        description="Each keeps some top-level folders of this organization's secrets. argus-control pushes it the rules, every minute, and collects its read log into the audit log. Values never pass through here unsealed."
        action={<Button size="xs" leftSection={<IconPlus size={14} />} onClick={() => setEditing('new')}>Add server</Button>}
        flush
      >
        {v.servers.length === 0 ? (
          <Text size={FS.body} c="dimmed" p="md">
            No secrets servers yet. Install argus-secret on a server of your own, then add it here.
          </Text>
        ) : (
          <Table.ScrollContainer minWidth={860}>
            <Table verticalSpacing="sm" horizontalSpacing="md">
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>Server</Table.Th>
                  <Table.Th>Folders</Table.Th>
                  <Table.Th>Secrets</Table.Th>
                  <Table.Th>Rules and log</Table.Th>
                  <Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {v.servers.map((s) => (
                  <Table.Tr key={s.id}>
                    <Table.Td>
                      <Group gap={6}>
                        <Text size={FS.body} fw={600}>{s.name}</Text>
                        {s.mode === 'tunnel' && <Badge size="xs" variant="light" color="slate">Dials Argus</Badge>}
                      </Group>
                      <Text size="xs" c="dimmed">
                        {s.mode === 'tunnel' ? <>{s.connected ? 'Connected' : s.enrolled ? 'Not connected' : 'Not enrolled yet'}</> : <Mono>{s.url}</Mono>}
                        {s.version ? ` · ${s.version}` : ''}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4}>{s.folders.map((f) => <Badge key={f} size="sm" variant="default" tt="none">{f}/</Badge>)}</Group>
                    </Table.Td>
                    <Table.Td><Text size={FS.body}>{s.lastContactAt ? s.secretCount : '—'}</Text></Table.Td>
                    <Table.Td><InStep s={s} /></Table.Td>
                    <Table.Td>
                      <Group gap={6} justify="flex-end" wrap="nowrap">
                        {s.mode === 'tunnel' && !s.enrolled ? (
                          <Button size="xs" variant="default" loading={token.isPending && token.variables === s.id}
                            onClick={() => void run(async () => setEnrolment(await token.mutateAsync(s.id)), { failure: 'No token made' })}>
                            Enrolment token
                          </Button>
                        ) : (
                          <Button size="xs" variant="default" loading={sync.isPending && sync.variables === s.id}
                            onClick={() => void run(() => sync.mutateAsync(s.id), { failure: 'Not synced' })}>
                            Sync now
                          </Button>
                        )}
                        {s.logGap && <Button size="xs" variant="default" onClick={() => setResuming(s)}>Resume log</Button>}
                        <Button size="xs" variant="default" onClick={() => setEditing(s)}>Edit</Button>
                        <Button size="xs" variant="subtle" color="rose" onClick={() => setRemoving(s)}>Remove</Button>
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
      </SectionCard>
      <ControlKey v={v} />
      {editing && (
        <ServerForm v={v} server={editing === 'new' ? null : editing} onClose={() => setEditing(null)} onEnrol={setEnrolment} />
      )}
      {enrolment && <EnrolDialog e={enrolment} onClose={() => setEnrolment(null)} />}
      {removing && <RemoveServer s={removing} onClose={() => setRemoving(null)} />}
      {resuming && <ResumeLog s={resuming} onClose={() => setResuming(null)} />}
    </Stack>
  )
}

/** Whether rules and the log are current, or what is wrong. */
function InStep({ s }: { s: SecretsServer }) {
  if (s.logGap) {
    return (
      <Stack gap={0}>
        <Text size={FS.body} c={INK.denied} fw={500}>Read log has a gap</Text>
        <Text size="xs" c="dimmed" lineClamp={2} title={s.logGap}>{s.logGap}</Text>
      </Stack>
    )
  }
  const failedLast = s.lastErrorAt && (!s.lastContactAt || s.lastErrorAt > s.lastContactAt)
  if (failedLast) {
    return (
      <Stack gap={0}>
        <Text size={FS.body} c={INK.denied} fw={500}>Not reached</Text>
        <Text size="xs" c="dimmed" lineClamp={2} title={s.lastError}>{s.lastError} · {relTime(s.lastErrorAt ?? null)}</Text>
      </Stack>
    )
  }
  if (s.lastContactAt) {
    return (
      <Stack gap={0}>
        <Text size={FS.body} c={INK.verified} fw={500}>In step</Text>
        <Text size="xs" c="dimmed">Rules {s.rulesSerial}, log entry {s.logSeq} · {relTime(s.lastContactAt)}</Text>
      </Stack>
    )
  }
  return <Text size={FS.body} c="dimmed">Not reached yet</Text>
}

function ControlKey({ v }: { v: SecretsServersView }) {
  return (
    <SectionCard title="This organization's key"
      description="A server you reach at its address needs these in its secret.yaml: organization, and control_public_key. A server that dials Argus gets both when it enrols.">
      <Stack gap={SP.snug}>
        <Group gap="xs" wrap="nowrap">
          <Text size={FS.meta} c="dimmed" style={{ flexShrink: 0 }}>organization</Text>
          <Mono>{v.organization}</Mono>
        </Group>
        <Group gap="xs" wrap="nowrap">
          <Text size={FS.meta} c="dimmed" style={{ flexShrink: 0 }}>control_public_key</Text>
          <Box style={{ minWidth: 0, overflowWrap: 'anywhere' }}><Mono>{v.controlPublicKey}</Mono></Box>
          <CopyButton value={v.controlPublicKey}>
            {({ copied, copy }) => (
              <Button size="xs" variant="default" leftSection={<IconCopy size={14} />} onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>
            )}
          </CopyButton>
        </Group>
      </Stack>
    </SectionCard>
  )
}

function ServerForm({ v, server, onClose, onEnrol }: {
  v: SecretsServersView; server: SecretsServer | null; onClose: () => void; onEnrol: (e: Enrolment) => void
}) {
  const add = useAddSecretsServer()
  const change = useChangeSecretsServer()
  const [mode, setMode] = useState<'direct' | 'tunnel'>(server?.mode ?? 'tunnel')
  const [name, setName] = useState(server?.name ?? '')
  const [url, setUrl] = useState(server?.url ?? '')
  const [pin, setPin] = useState(server?.tlsPin ?? '')
  const [folders, setFolders] = useState<string[]>(server?.folders ?? [])
  const taken = new Set(v.servers.filter((s) => s.id !== server?.id).flatMap((s) => s.folders))
  const clash = folders.find((f) => taken.has(f.replace(/\/+$/, '')))
  const input: SecretsServerInput = {
    name: name.trim(), mode, folders: folders.map((f) => f.trim().replace(/\/+$/, '')),
    url: mode === 'direct' ? url.trim() : '', tlsPin: mode === 'direct' ? pin.trim() : '',
  }
  const ready = input.name !== '' && input.folders.length > 0 && !clash && (mode === 'tunnel' || input.url.startsWith('https://'))
  const save = () => void run(async () => {
    if (server) {
      await change.mutateAsync({ id: server.id, input })
      return
    }
    const out = await add.mutateAsync(input)
    if ('enrollToken' in out) onEnrol(out)
  }, {
    failure: server ? 'Server not changed' : 'Server not added',
    success: [server ? 'Saved' : 'Added', mode === 'tunnel' && !server ? `Enrol ${input.name} with the token below.` : `${input.name} gets its rules within a minute.`],
  }).then((ok) => { if (ok) onClose() })

  return (
    <Modal opened onClose={onClose} title={server ? `Edit ${server.name}` : 'Add a secrets server'} size="lg">
      <Stack gap="sm">
        {!server && (
          <SegmentedControl value={mode} onChange={(x) => setMode(x as 'direct' | 'tunnel')} data={[
            { value: 'tunnel', label: 'It dials Argus' },
            { value: 'direct', label: 'Argus reaches its address' },
          ]} />
        )}
        <Text size={FS.meta} c="dimmed">
          {mode === 'tunnel'
            ? 'argus-secret connects out to argus-control and needs no port open to it. You get a one-time token to enrol it with.'
            : 'argus-control and the agents call it at an https:// address: its own, or a proxy such as Cloudflare Tunnel in front of it.'}
        </Text>
        <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80} data-autofocus />
        {mode === 'direct' && (
          <>
            <TextInput label="Address" placeholder="https://secrets.example.com:8460" value={url} onChange={(e) => setUrl(e.currentTarget.value)} />
            <TextInput label="Certificate pin" description="The sha256:… argus-secret printed when it started. Empty for a certificate the system trusts, such as a proxy's."
              value={pin} onChange={(e) => setPin(e.currentTarget.value)} />
          </>
        )}
        <TagsInput label="Top-level folders it keeps" description="Such as prod or payments. A folder lives on one server."
          value={folders} onChange={setFolders} error={clash ? `${clash} is already kept by another server` : undefined} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!ready} loading={add.isPending || change.isPending} onClick={save}>
            {server ? 'Save' : mode === 'tunnel' ? 'Add and make token' : 'Check and add'}
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function EnrolDialog({ e, onClose }: { e: Enrolment; onClose: () => void }) {
  const cmd = enrolCommand(e.enrollToken)
  return (
    <Modal opened onClose={onClose} title={`Enrol ${e.name}`} size="lg">
      <Stack gap="sm">
        <Alert color="amber" p="xs">
          <Text size={FS.body}>
            Run this on the server within the hour{e.enrollExpiresAt ? ` (until ${new Date(e.enrollExpiresAt).toLocaleTimeString()})` : ''}.
            The token works once and is not shown again.
          </Text>
        </Alert>
        <Code block style={{ overflowWrap: 'anywhere', whiteSpace: 'pre-wrap' }}>{cmd}</Code>
        <Text size={FS.meta} c="dimmed">
          Add <Mono>--pin sha256:…</Mono> or <Mono>--ca file</Mono> when the server does not trust this address's certificate.
          Set <Mono>listen: off</Mono> in its secret.yaml to serve only over the connection to Argus.
        </Text>
        <Group justify="flex-end">
          <CopyButton value={cmd}>
            {({ copied, copy }) => <Button variant="default" leftSection={<IconCopy size={14} />} onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>}
          </CopyButton>
          <Button onClick={onClose}>Done</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function RemoveServer({ s, onClose }: { s: SecretsServer; onClose: () => void }) {
  const remove = useRemoveSecretsServer()
  return (
    <Modal opened onClose={onClose} title={`Remove ${s.name}?`}>
      <Stack gap="sm">
        <Text size={FS.body}>
          Its secrets stay on it, but argus-control stops pushing it rules, so it refuses every read once they go stale, within 15 minutes.
          Servers and apps reading from {s.folders.map((f) => `${f}/`).join(', ')} stop getting values.
        </Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" loading={remove.isPending}
            onClick={() => void run(() => remove.mutateAsync(s.id), { failure: 'Server not removed', success: ['Removed', `${s.name} is no longer kept in step.`] })
              .then((ok) => { if (ok) onClose() })}>
            Remove server
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function ResumeLog({ s, onClose }: { s: SecretsServer; onClose: () => void }) {
  const resume = useResumeSecretsLog()
  const [reason, setReason] = useState('')
  return (
    <Modal opened onClose={onClose} title={`Resume ${s.name}'s read log?`}>
      <Stack gap="sm">
        <Text size={FS.body}>
          Its log stopped following on from what Argus had collected: {s.logGap}. Resuming takes it from where the server is now.
          What is missing stays missing, and the audit log records that you resumed it and why.
        </Text>
        <TextInput label="Why is the gap understood?" value={reason} onChange={(e) => setReason(e.currentTarget.value)} maxLength={500} />
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button disabled={!reason.trim()} loading={resume.isPending}
            onClick={() => void run(() => resume.mutateAsync({ id: s.id, reason: reason.trim() }), { failure: 'Not resumed', success: ['Resumed', `${s.name}'s log is collected again.`] })
              .then((ok) => { if (ok) onClose() })}>
            Resume log
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
