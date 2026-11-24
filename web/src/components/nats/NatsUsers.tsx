import {
  ActionIcon, Alert, Badge, Box, Button, Card, Checkbox, Code, CopyButton, Drawer, Group, Modal, SegmentedControl, Select, Stack, Table,
  TagsInput, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { IconAlertTriangle, IconCheck, IconCopy, IconDownload, IconSearch, IconUserPlus } from '@tabler/icons-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import { ChoiceCards, ConfirmTyped, MONO, Section } from '~/components/brokerusers/parts'
import { dataOf, useRedisChange } from '~/components/redis/useRedisChange'
import type { StoreSession } from '~/components/useStoreSession'
import { canMakeUserKeys, credsFile, newUserKey } from '~/lib/nkeys'
import {
  DAY_CHOICES, DEFAULT_DAYS, PRESETS, connectSnippets, credsName, expiry, presetSpec, revokeCommand, ruleLines, shortKey, userFindings, userProblems,
  type NatsAppUser, type NatsAppUserSpec, type NatsIssued, type NatsJetStream, type NatsPreset, type NatsUsersData,
} from '~/lib/natsUsers'
import { notifyError, notifyOk } from '~/lib/notify'
import { FS } from '~/theme'
import type { Database } from '~/types/domain'

/*
The users this account's apps sign in with. In operator mode the server
stores no users: Argus signs one (a JWT with what it may publish and
subscribe to, and an expiry) for a key this browser makes, and hands the
person a .creds file with the JWT and the key's seed. The seed never leaves
the browser and Argus does not keep it, so the file is offered once.

The list is what Argus signed for this account, with each user's connections
now, and the users connected now that Argus did not sign. A user cannot be
changed or taken back once signed: sign a new one, and let the old one
expire or have the operator revoke it with nsc.
*/

function Rules({ spec }: { spec: NatsAppUserSpec }) {
  return (
    <Stack gap={2}>
      {ruleLines(spec).map((l) => (
        <Text key={l.what} size={FS.micro}><Text span c="dimmed" size={FS.micro}>{l.what}</Text> <Text span ff="monospace" size={FS.micro}>{l.words}</Text></Text>
      ))}
    </Stack>
  )
}

function Copy({ value, label }: { value: string; label: string }) {
  return (
    <CopyButton value={value}>
      {({ copied, copy }) => (
        <Tooltip label={copied ? 'Copied' : label}>
          <ActionIcon size="sm" variant="subtle" onClick={copy} aria-label={label}>{copied ? <IconCheck size={14} /> : <IconCopy size={14} />}</ActionIcon>
        </Tooltip>
      )}
    </CopyButton>
  )
}

function download(name: string, text: string) {
  const url = URL.createObjectURL(new Blob([text], { type: 'application/octet-stream' }))
  const a = document.createElement('a')
  a.href = url
  a.download = name
  a.click()
  setTimeout(() => URL.revokeObjectURL(url), 1000)
}

function madeWords(u: NatsAppUser): string {
  if (!u.inArgus) return 'Outside Argus'
  const who = u.by?.split('@')[0] ?? 'someone'
  return u.issuedAt ? `Argus · ${who}, ${new Date(u.issuedAt).toLocaleDateString(undefined, { day: 'numeric', month: 'short' })}` : `Argus · ${who}`
}

export function NatsUsers({ s, database, narrow }: { s: StoreSession; database: Database; narrow: boolean }) {
  const account = database.dbname
  const ops = s.ready?.ops ?? []
  const admin = ops.includes('user_set')
  const { change, dialog } = useRedisChange(s, database)
  const [data, setData] = useState<NatsUsersData | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [q, setQ] = useState('')
  const [creating, setCreating] = useState<{ name?: string; spec?: NatsAppUserSpec } | null>(null)
  const [open, setOpen] = useState<string | null>(null)
  const [issued, setIssued] = useState<{ name: string; creds: string } | null>(null)

  const load = useCallback(async () => {
    const a = await s.ask({ op: 'users' })
    const d = dataOf<NatsUsersData>(a)
    if (!d) setErr(a.error ?? 'could not list the users')
    else { setErr(null); setData(d) }
  }, [s])
  useEffect(() => { if (s.status === 'ready') void load() }, [s.status, load])

  const rows = useMemo(() => (data?.users ?? []).filter((u) => !q.trim() || u.name.toLowerCase().includes(q.trim().toLowerCase()) || u.publicKey.includes(q.trim().toUpperCase())), [data, q])
  const findings = useMemo(() => userFindings(data?.users ?? []), [data])

  if (!data) return err ? <Alert color="rose" variant="light">{err}</Alert> : <Text size={FS.meta} c="dimmed">Reading the users…</Text>
  const user = open ? data.users.find((u) => u.publicKey === open) ?? null : null
  const url = `nats://${database.hostname}:${database.port || 4222}`
  return (
    <Stack gap="md">
      {findings.length > 0 && (
        <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />} title={`${findings.length} user${findings.length === 1 ? '' : 's'} to renew`}>
          <Stack gap={2}>{findings.map((f) => (
            <Group key={f.key} gap="xs"><Text size={FS.meta}>{f.text}</Text><Button size="compact-xs" variant="light" color="amber" onClick={() => setOpen(f.key)}>Open</Button></Group>
          ))}</Stack>
        </Alert>
      )}
      <Card withBorder radius="md" padding={0}>
        <Group p="md" gap="sm" align="flex-start" wrap={narrow ? 'wrap' : 'nowrap'} style={{ borderBottom: '1px solid var(--color-line)' }}>
          <Box style={{ flex: 1, minWidth: 220 }}>
            <Text fw={600} size={FS.body}>Users of apps in {account}</Text>
            <Text size={FS.micro} c="dimmed">
              The users Argus signed for this account and the ones connected now that it did not. Each signed user expires;
              to cut one off sooner, the operator revokes it with nsc.
              {data.connsError && ` ${data.connsError}.`}
            </Text>
          </Box>
          <TextInput size="xs" placeholder="Search users" leftSection={<IconSearch size={12} />} value={q} onChange={(e) => setQ(e.currentTarget.value)}
            aria-label="Search users" w={narrow ? '100%' : 220} />
          {admin && <Button size="xs" leftSection={<IconUserPlus size={14} />} onClick={() => setCreating({})}>New user</Button>}
        </Group>
        {rows.length === 0 && (
          <Text size={FS.meta} c="dimmed" p="md">
            {data.users.length === 0 ? 'Argus has signed no users for this account, and no app is connected.' : 'No user matches.'}
          </Text>
        )}
        {narrow ? (
          <Stack gap={0}>
            {rows.map((u) => {
              const e = expiry(u.expires)
              return (
                <UnstyledButton key={u.publicKey} onClick={() => setOpen(u.publicKey)} p="sm" style={{ borderBottom: '1px solid var(--color-line)' }}>
                  <Group gap={6} mb={4}>
                    <Text ff="monospace" fw={600} size={FS.meta}>{u.name || 'unnamed'}</Text>
                    {u.inArgus && <Badge size="xs" variant="light" color={e.color}>{e.text}</Badge>}
                  </Group>
                  {u.inArgus && <Rules spec={u.spec} />}
                  <Text size={FS.micro} c="dimmed" mt={4}>{u.connections} connection{u.connections === 1 ? '' : 's'} · {madeWords(u)}</Text>
                </UnstyledButton>
              )
            })}
          </Stack>
        ) : rows.length > 0 && (
          <Table.ScrollContainer minWidth={820}>
            <Table verticalSpacing={8} fz="sm" highlightOnHover>
              <Table.Thead>
                <Table.Tr><Table.Th>User</Table.Th><Table.Th>May</Table.Th><Table.Th>Valid</Table.Th><Table.Th>Connections</Table.Th><Table.Th>Made</Table.Th></Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {rows.map((u) => {
                  const e = expiry(u.expires)
                  return (
                    <Table.Tr key={u.publicKey} onClick={() => setOpen(u.publicKey)} style={{ cursor: 'pointer' }}>
                      <Table.Td>
                        <Text ff="monospace" fw={600} size={FS.meta}>{u.name || 'unnamed'}</Text>
                        <Text ff="monospace" size={FS.micro} c="dimmed">{shortKey(u.publicKey)}</Text>
                      </Table.Td>
                      <Table.Td>{u.inArgus ? <Rules spec={u.spec} /> : <Text size={FS.micro} c="dimmed">Signed outside Argus</Text>}</Table.Td>
                      <Table.Td>{u.inArgus ? <Badge size="sm" variant="light" color={e.color}>{e.text}</Badge> : <Text size={FS.micro} c="dimmed">—</Text>}</Table.Td>
                      <Table.Td>{u.connections}</Table.Td>
                      <Table.Td><Text size={FS.micro} c="dimmed">{madeWords(u)}</Text></Table.Td>
                    </Table.Tr>
                  )
                })}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
      </Card>
      {user && (
        <UserDrawer u={user} account={account} admin={admin} narrow={narrow} onClose={() => setOpen(null)}
          onRenew={() => { setOpen(null); setCreating({ name: user.name, spec: user.spec }) }}
          onForget={async () => {
            const a = await change({ op: 'user_del', brokerUser: { name: user.name, publicKey: user.publicKey } })
            if (a.type === 'result') { notifyOk('Taken off the list', `${user.name} is no longer listed. It is in the session’s recording.`); setOpen(null); void load() }
            else if (!a.notApproved) notifyError('Not done', new Error(a.error ?? 'failed'))
          }} />
      )}
      {creating && (
        <NewUser initialName={creating.name} initialSpec={creating.spec} narrow={narrow} onClose={() => setCreating(null)}
          onSign={async (name, spec) => {
            const key = await newUserKey()
            const a = await change({ op: 'user_set', brokerUser: { name, create: true, publicKey: key.publicKey, nats: spec } })
            const d = dataOf<NatsIssued>(a)
            if (!d) {
              if (!a.notApproved) notifyError('Not signed', new Error(a.error ?? 'failed'))
              return false
            }
            setCreating(null)
            setIssued({ name, creds: credsFile(d.jwt, key.seed) })
            void load()
            return true
          }} />
      )}
      {issued && <Issued name={issued.name} creds={issued.creds} file={credsName(account, issued.name)} url={url} onClose={() => setIssued(null)} />}
      {dialog}
    </Stack>
  )
}

function UserDrawer({ u, account, admin, narrow, onClose, onRenew, onForget }: {
  u: NatsAppUser; account: string; admin: boolean; narrow: boolean; onClose: () => void; onRenew: () => void; onForget: () => Promise<void>
}) {
  const [forgetting, setForgetting] = useState(false)
  const e = expiry(u.expires)
  const revoke = revokeCommand(account, u.publicKey)
  return (
    <Drawer opened onClose={onClose} position="right" size={narrow ? '100%' : 520} title={<Text fw={600} ff="monospace">{u.name || 'unnamed'}</Text>}>
      <Stack gap="md">
        <Section title="Public key">
          <Group gap={6} wrap="nowrap"><Code style={{ flex: 1, overflowWrap: 'anywhere' }}>{u.publicKey}</Code><Copy value={u.publicKey} label="Copy key" /></Group>
        </Section>
        {u.inArgus ? (
          <>
            <Section title="May" hint={`${u.connections} connection${u.connections === 1 ? '' : 's'} now`}><Rules spec={u.spec} /></Section>
            <Section title="Valid">
              <Text size={FS.meta}><Badge size="sm" variant="light" color={e.color}>{e.text}</Badge>{u.expires && ` · until ${new Date(u.expires).toLocaleString()}`}</Text>
              <Text size={FS.micro} c="dimmed">{madeWords(u)}. A signed user cannot be changed; sign a new one with other rules and move the app to its .creds file.</Text>
            </Section>
          </>
        ) : (
          <Text size={FS.meta} c="dimmed">Signed outside Argus, with nsc or another tool, and connected now ({u.connections}). Argus does not know its rules or expiry.</Text>
        )}
        <Section title="Cut it off before it expires" hint="the operator runs this">
          <Group gap={6} wrap="nowrap" align="flex-start"><Code block style={{ flex: 1, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{revoke}</Code><Copy value={revoke} label="Copy commands" /></Group>
          <Text size={FS.micro} c="dimmed">Revoking changes the account, which only the operator’s key signs. Argus does not hold that key.</Text>
        </Section>
        {admin && u.inArgus && (
          <Group gap="xs">
            <Button size="xs" onClick={onRenew}>Sign a new one like this</Button>
            <Button size="xs" variant="default" color="rose" onClick={() => setForgetting(true)}>Take off the list…</Button>
          </Group>
        )}
      </Stack>
      <ConfirmTyped opened={forgetting} name={u.name} title={`Take ${u.name} off the list`} action="Take off the list"
        text={`Argus stops listing ${u.name}. Its .creds file still signs in until ${u.expires ? new Date(u.expires).toLocaleDateString() : 'it expires'} unless the operator revokes it with the commands above.`}
        onCancel={() => setForgetting(false)} onConfirm={async () => { await onForget(); setForgetting(false) }} />
    </Drawer>
  )
}

const JETSTREAM: { value: NatsJetStream; label: string }[] = [{ value: '', label: 'None' }, { value: 'use', label: 'Use' }, { value: 'manage', label: 'Manage' }]

function NewUser({ initialName, initialSpec, narrow, onClose, onSign }: {
  initialName?: string; initialSpec?: NatsAppUserSpec; narrow: boolean; onClose: () => void; onSign: (name: string, spec: NatsAppUserSpec) => Promise<boolean>
}) {
  const [name, setName] = useState(initialName ?? '')
  const [preset, setPreset] = useState<NatsPreset>(initialSpec ? 'custom' : 'publisher')
  const [subjects, setSubjects] = useState<string[]>([])
  const [spec, setSpec] = useState<NatsAppUserSpec>(initialSpec ?? presetSpec('publisher', [], DEFAULT_DAYS))
  const [busy, setBusy] = useState(false)
  const [keysOk, setKeysOk] = useState<boolean | null>(null)
  useEffect(() => { void canMakeUserKeys().then(setKeysOk) }, [])
  const pick = (p: NatsPreset) => { setPreset(p); if (p !== 'custom') setSpec({ ...presetSpec(p, subjects, spec.days), sources: spec.sources }) }
  const setSubjectsFor = (l: string[]) => { setSubjects(l); if (preset !== 'custom') setSpec({ ...presetSpec(preset, l, spec.days), sources: spec.sources }) }
  const problems = userProblems(name, spec)
  const set = (p: Partial<NatsAppUserSpec>) => { setPreset('custom'); setSpec({ ...spec, ...p }) }
  return (
    <Modal opened onClose={onClose} title={initialName ? `Sign a new ${initialName}` : 'New user for an app'} size="lg" fullScreen={narrow}>
      <Stack gap="md">
        {keysOk === false && <Alert color="rose" variant="light">This browser cannot make NATS keys (Ed25519). Use a current Chrome, Firefox, Safari or Edge.</Alert>}
        <TextInput label="Name" description="As the app is known; it shows in Connections." value={name} onChange={(e) => setName(e.currentTarget.value.trim())} styles={MONO} data-autofocus />
        <Section title="What the app does">
          <ChoiceCards value={preset} onChange={pick} options={PRESETS} cols={5} />
          {preset !== 'custom' && (
            <TagsInput label="Its subjects" description="Wildcards work: orders.* is one token, orders.> the rest." value={subjects} onChange={setSubjectsFor}
              placeholder="orders.>" styles={MONO} />
          )}
        </Section>
        {preset === 'custom' && (
          <Section title="Rules">
            <TagsInput label="Publishes to" value={spec.pubAllow} onChange={(v) => set({ pubAllow: v })} styles={MONO} />
            <TagsInput label="…except" value={spec.pubDeny ?? []} onChange={(v) => set({ pubDeny: v })} styles={MONO} />
            <TagsInput label="Subscribes to" value={spec.subAllow} onChange={(v) => set({ subAllow: v })} styles={MONO} />
            <TagsInput label="…except" value={spec.subDeny ?? []} onChange={(v) => set({ subDeny: v })} styles={MONO} />
            <Checkbox label="Makes requests (reads their replies)" checked={Boolean(spec.requests)} onChange={(e) => set({ requests: e.currentTarget.checked })} />
            <Checkbox label="Answers requests" checked={Boolean(spec.replies)} onChange={(e) => set({ replies: e.currentTarget.checked })} />
            <Box>
              <Text size={FS.meta} fw={500} mb={4}>JetStream</Text>
              <SegmentedControl size="xs" value={spec.jetstream ?? ''} onChange={(v) => set({ jetstream: v as NatsJetStream })} data={JETSTREAM} />
              <Text size={FS.micro} c="dimmed" mt={4}>Use reads streams and makes and pulls from consumers; Manage also makes and deletes streams.</Text>
            </Box>
          </Section>
        )}
        <Group grow align="flex-start">
          <Select label="Valid for" value={String(spec.days)} onChange={(v) => v && setSpec({ ...spec, days: Number(v) })} allowDeselect={false}
            data={DAY_CHOICES.map((d) => ({ value: String(d), label: d === 365 ? '1 year' : `${d} days` }))}
            description="Then it stops signing in; sign a new one before." />
          <TagsInput label="Connects only from" description="Optional: addresses or ranges like 10.0.0.0/8." value={spec.sources ?? []}
            onChange={(v) => setSpec({ ...spec, sources: v })} styles={MONO} />
        </Group>
        <Box p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
          <Text size={FS.meta} fw={600} mb={4}>{name || 'The user'} will be able to</Text>
          <Rules spec={spec} />
        </Box>
        {problems.length > 0 && name && <Stack gap={2}>{problems.map((p) => <Text key={p} size={FS.micro} c="rose">{p}</Text>)}</Stack>}
        <Text size={FS.micro} c="dimmed">This browser makes the user’s key. Only its public half is sent; the secret goes in the .creds file you download next, once.</Text>
        <Group justify="flex-end">
          <Button variant="default" size="xs" onClick={onClose}>Cancel</Button>
          <Button size="xs" disabled={problems.length > 0 || keysOk === false} loading={busy}
            onClick={async () => { setBusy(true); try { await onSign(name, spec) } finally { setBusy(false) } }}>Sign user</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function Issued({ name, creds, file, url, onClose }: { name: string; creds: string; file: string; url: string; onClose: () => void }) {
  const [saved, setSaved] = useState(false)
  const [lang, setLang] = useState(0)
  const snippets = connectSnippets(url, file)
  return (
    <Modal opened onClose={() => { if (saved) onClose() }} title={`${name} is signed`} size="lg" closeOnClickOutside={false} withCloseButton={saved}>
      <Stack gap="md">
        <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />}>
          Download the .creds file now. It holds the user’s secret, which Argus does not keep: once this closes, the only way back is to sign a new user.
        </Alert>
        <Group gap="xs">
          <Button leftSection={<IconDownload size={14} />} onClick={() => { download(file, creds); setSaved(true) }}>Download {file}</Button>
          <CopyButton value={creds}>
            {({ copied, copy }) => <Button variant="default" leftSection={copied ? <IconCheck size={14} /> : <IconCopy size={14} />} onClick={() => { copy(); setSaved(true) }}>{copied ? 'Copied' : 'Copy file'}</Button>}
          </CopyButton>
        </Group>
        <Section title="Connect with it">
          <SegmentedControl size="xs" value={String(lang)} onChange={(v) => setLang(Number(v))} data={snippets.map((s, i) => ({ value: String(i), label: s.label }))} />
          <Group gap={6} wrap="nowrap" align="flex-start"><Code block style={{ flex: 1, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{snippets[lang]!.code}</Code><Copy value={snippets[lang]!.code} label="Copy code" /></Group>
          <Text size={FS.micro} c="dimmed">Keep the file where only the app can read it, as a password.</Text>
        </Section>
        <Group justify="flex-end"><Button size="xs" variant="default" disabled={!saved} onClick={onClose}>I have saved it</Button></Group>
      </Stack>
    </Modal>
  )
}
