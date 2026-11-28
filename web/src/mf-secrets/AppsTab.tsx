import { useState } from 'react'
import { Alert, Badge, Button, Code, CopyButton, Group, Modal, NumberInput, Stack, Table, Text, TextInput } from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconCopy, IconPlugConnected } from '@tabler/icons-react'
import { DataTable, EmptyState, FS, INK, Mono, SP, relTime, run } from '@argus/shell'
import { appsQuery, useMakeApp, useRevokeApp, type SecretApp } from './api'

export function AppsTab({ adding, onAdded }: { adding: boolean; onAdded: () => void }) {
  const { data, isLoading, error } = useQuery(appsQuery())
  const [revoking, setRevoking] = useState<SecretApp | null>(null)
  return (
    <Stack gap="md">
      {error && <Alert color="rose"><Text size={FS.body}>Could not read the app tokens: {error.message}</Text></Alert>}
      <DataTable
        columns={['App', 'Token', 'Expires', 'Made by', '']}
        minWidth={720}
        loading={isLoading}
        isEmpty={!!data && data.length === 0}
        empty={<EmptyState compact icon={IconPlugConnected} title="No app tokens" description="Servers with an agent need none: the agent reads for them." />}
      >
        {data?.map((a) => {
          const over = a.revokedAt || new Date(a.expiresAt).getTime() < Date.now()
          return (
            <Table.Tr key={a.id}>
              <Table.Td><Text size={FS.meta} fw={600}>{a.name}</Text><Text size={FS.micro} c="dimmed"><Mono>{a.id}</Mono></Text></Table.Td>
              <Table.Td><Mono>••••{a.tokenTail}</Mono></Table.Td>
              <Table.Td>
                {a.revokedAt
                  ? <Text size={FS.meta} c={INK.denied}>Revoked {relTime(a.revokedAt)}{a.revokedBy ? ` by ${a.revokedBy}` : ''}</Text>
                  : <Text size={FS.meta} c={over ? INK.denied : undefined}>{over ? 'Expired' : 'In'} {relTime(a.expiresAt)}</Text>}
              </Table.Td>
              <Table.Td><Text size={FS.meta}>{a.createdBy}</Text></Table.Td>
              <Table.Td>
                <Group gap={SP.snug} justify="flex-end" wrap="nowrap">
                  {!over && <Button size="xs" variant="subtle" color="rose" onClick={() => setRevoking(a)}>Revoke</Button>}
                  {over && <Badge size="sm" variant="light" color="slate">Not usable</Badge>}
                </Group>
              </Table.Td>
            </Table.Tr>
          )
        })}
      </DataTable>
      {adding && <MakeApp onClose={onAdded} />}
      {revoking && <Revoke app={revoking} onClose={() => setRevoking(null)} />}
    </Stack>
  )
}

function MakeApp({ onClose }: { onClose: () => void }) {
  const make = useMakeApp()
  const [name, setName] = useState('')
  const [days, setDays] = useState<number | string>(90)
  const [made, setMade] = useState<(SecretApp & { token: string }) | null>(null)
  return (
    <Modal opened onClose={onClose} title={made ? `Token for ${made.name}` : 'New app token'}>
      {made ? (
        <Stack gap="sm">
          <Alert color="amber" p="xs"><Text size={FS.body}>Copy it now. It is not kept anywhere and is never shown again.</Text></Alert>
          <Code block style={{ overflowWrap: 'anywhere', whiteSpace: 'pre-wrap' }}>{made.token}</Code>
          <Text size={FS.meta} c="dimmed">
            The app sends <Mono>Authorization: App …</Mono> to its secrets server’s <Mono>/v1/read</Mono>. Add a rule naming app <Mono>{made.id}</Mono> for what it may read.
          </Text>
          <Group justify="flex-end">
            <CopyButton value={made.token}>
              {({ copied, copy }) => <Button variant="default" leftSection={<IconCopy size={14} />} onClick={copy}>{copied ? 'Copied' : 'Copy'}</Button>}
            </CopyButton>
            <Button onClick={onClose}>Done</Button>
          </Group>
        </Stack>
      ) : (
        <Stack gap="sm">
          <TextInput label="App" description="What uses it, such as billing-worker." value={name} onChange={(e) => setName(e.currentTarget.value)} maxLength={80} data-autofocus />
          <NumberInput label="Good for, in days" min={1} max={365} value={days} onChange={setDays} />
          <Group justify="flex-end">
            <Button variant="default" onClick={onClose}>Cancel</Button>
            <Button disabled={!name.trim()} loading={make.isPending}
              onClick={() => void run(async () => setMade(await make.mutateAsync({ name: name.trim(), days: Number(days) || 90 })), { failure: 'Token not made' })}>
              Make token
            </Button>
          </Group>
        </Stack>
      )}
    </Modal>
  )
}

function Revoke({ app, onClose }: { app: SecretApp; onClose: () => void }) {
  const revoke = useRevokeApp()
  return (
    <Modal opened onClose={onClose} title={`Revoke ${app.name}?`}>
      <Stack gap="sm">
        <Text size={FS.body}>Its token stops working on every secrets server within a minute.</Text>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button color="rose" loading={revoke.isPending}
            onClick={() => void run(() => revoke.mutateAsync(app.id), { failure: 'Not revoked', success: ['Revoked', `${app.name}’s token stops working within a minute.`] })
              .then((ok) => { if (ok) onClose() })}>
            Revoke token
          </Button>
        </Group>
      </Stack>
    </Modal>
  )
}
