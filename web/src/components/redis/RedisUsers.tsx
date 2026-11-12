import { useCallback, useEffect, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Box, Button, Code, CopyButton, Drawer, Group, Modal, PasswordInput, SegmentedControl, Select,
  Stack, Switch, Table, Text, TextInput, Textarea, Tooltip,
} from '@mantine/core'
import { IconCheck, IconCopy, IconPlus, IconTrash, IconUserShield, IconX } from '@tabler/icons-react'
import { FS, SP } from '~/theme'
import { SectionCard } from '~/components/page'
import { generatePassword } from '~/lib/password'
import type { StoreRequest, StoreSession } from '~/components/useStoreSession'
import type { DBLevel, Database } from '~/types/domain'
import {
  USER_TEMPLATES, keyRuleWords, levelRules, ruleKind, rulesProblem, sha256Hex, userFinding, userNameProblem,
  type RedisDryRun, type RedisUser, type RedisUserResult, type RedisUsersData,
} from '~/lib/redisManage'
import { dataOf, type ChangeAnswer } from '~/components/redis/useRedisChange'

/*
The server's own Redis users (ACL), for the applications that sign in to it,
beside the users Argus makes for each session. Passwords are made or typed in
the browser and only their SHA-256 leaves it, so neither the control plane nor
the agent ever has the password; Redis keeps hashes too. Argus's own users and
the one the agent signs in as are shown, not changed here.
*/

export function RedisUsers({ s, database, canManage, canCheck, change }: {
  s: StoreSession
  database: Database
  canManage: boolean
  canCheck: boolean
  change: (req: StoreRequest) => Promise<ChangeAnswer>
}) {
  const [data, setData] = useState<RedisUsersData | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [editing, setEditing] = useState<RedisUser | null>(null)
  const [creating, setCreating] = useState(false)
  const [deleting, setDeleting] = useState<RedisUser | null>(null)
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null)

  const load = useCallback(async () => {
    const a = await s.ask({ op: 'acl_list' })
    const d = dataOf<RedisUsersData>(a)
    if (!d) setErr(a.error ?? 'could not list the users')
    else { setErr(null); setData(d) }
  }, [s])
  useEffect(() => { if (s.status === 'ready') void load() }, [s.status, load])

  const saved = (a: ChangeAnswer, what: string): boolean => {
    if (a.notApproved) { setNote({ ok: false, text: 'Nothing changed: it was not approved.' }); return false }
    const r = dataOf<RedisUserResult>(a)
    if (!r) { setNote({ ok: false, text: a.error ?? 'failed' }); return false }
    setNote({ ok: !r.saveError, text: `${what}${r.saveError ? ` It is live, but saving it failed: ${r.saveError}` : r.saved ? ` Saved with ${r.saved}.` : ''}` })
    void load()
    return true
  }

  if (!data) return <Box p="md">{err ? <Alert color="rose">{err}</Alert> : <Text size={FS.meta} c="dimmed">Reading the users…</Text>}</Box>
  const own = data.users.filter((u) => !u.argus)
  const sessions = data.users.filter((u) => u.argus)

  return (
    <Stack gap="md" p="md">
      {note && <Alert color={note.ok ? 'teal' : 'rose'} withCloseButton onClose={() => setNote(null)}>{note.text}</Alert>}
      <SectionCard title="Server users" icon={IconUserShield}
        description={data.aclFile ? 'Kept in the server’s ACL file' : 'Kept in redis.conf (no aclfile is set)'}
        action={canManage ? <Button size="compact-xs" leftSection={<IconPlus size={12} />} onClick={() => setCreating(true)}>New user</Button> : undefined} flush>
        <Table verticalSpacing={6} fz="xs">
          <Table.Thead>
            <Table.Tr><Table.Th>User</Table.Th><Table.Th>Passwords</Table.Th><Table.Th>Can reach</Table.Th><Table.Th ta="right">Signed in</Table.Th>{canManage && <Table.Th w={70} />}</Table.Tr>
          </Table.Thead>
          <Table.Tbody>
            {own.map((u) => {
              const f = userFinding(u)
              return (
                <Table.Tr key={u.name}>
                  <Table.Td>
                    <Group gap={6} wrap="nowrap">
                      <Text size="xs" ff="monospace" fw={600}>{u.name}</Text>
                      <Badge size="xs" variant="light" color={u.on ? 'teal' : 'slate'}>{u.on ? 'on' : 'off'}</Badge>
                      {u.self && <Badge size="xs" variant="outline" color="slate">Argus signs in as this</Badge>}
                    </Group>
                    {f && <Text size={FS.micro} c={f.tone}>{f.text}</Text>}
                  </Table.Td>
                  <Table.Td>{u.nopass ? <Badge size="xs" color="rose" variant="light">none needed</Badge> : `${u.passwords.length}`}</Table.Td>
                  <Table.Td>
                    <Text size={FS.micro} ff="monospace" lineClamp={2}>{[...u.keys, ...u.channels].join(' ') || 'no keys'} · {u.commands.join(' ')}</Text>
                  </Table.Td>
                  <Table.Td ta="right">{u.clients}</Table.Td>
                  {canManage && (
                    <Table.Td>
                      {!u.self && (
                        <Group gap={2} wrap="nowrap">
                          <Button size="compact-xs" variant="subtle" onClick={() => setEditing(u)}>Edit</Button>
                          {u.name !== 'default' && <ActionIcon size="sm" variant="subtle" color="rose" aria-label={`Delete ${u.name}`} onClick={() => setDeleting(u)}><IconTrash size={12} /></ActionIcon>}
                        </Group>
                      )}
                    </Table.Td>
                  )}
                </Table.Tr>
              )
            })}
          </Table.Tbody>
        </Table>
      </SectionCard>

      <SectionCard title="Argus session users" description="One per person and level while a session is open; made and removed by Argus" flush>
        <Table verticalSpacing={4} fz="xs">
          <Table.Tbody>
            {sessions.map((u) => (
              <Table.Tr key={u.name}><Table.Td ff="monospace">{u.name}</Table.Td><Table.Td ff="monospace" c="dimmed">{u.commands.join(' ')}</Table.Td><Table.Td ta="right">{u.clients} signed in</Table.Td></Table.Tr>
            ))}
            {sessions.length === 0 && <Table.Tr><Table.Td><Text size={FS.meta} c="dimmed">None open.</Text></Table.Td></Table.Tr>}
          </Table.Tbody>
        </Table>
      </SectionCard>

      {canCheck && <LevelCheck s={s} database={database} />}

      {creating && (
        <UserForm s={s} onClose={() => setCreating(false)} existing={data.users.map((u) => u.name)}
          onSave={async (req, what) => { const ok = saved(await change(req), what); if (ok) setCreating(false); return ok }} />
      )}
      {editing && (
        <UserForm s={s} user={editing} onClose={() => setEditing(null)} existing={[]}
          onSave={async (req, what) => { const ok = saved(await change(req), what); if (ok) setEditing(null); return ok }} />
      )}
      <DeleteUser user={deleting} onClose={() => setDeleting(null)}
        onDelete={async (u) => { const ok = saved(await change({ op: 'acl_deluser', user: u.name }), `${u.name} was deleted.`); if (ok) setDeleting(null) }} />
    </Stack>
  )
}

function splitRules(text: string): string[] {
  return text.split(/\s+/).map((r) => r.trim()).filter(Boolean)
}

function UserForm({ s, user, existing, onClose, onSave }: {
  s: StoreSession
  user?: RedisUser
  existing: string[]
  onClose: () => void
  onSave: (req: StoreRequest, what: string) => Promise<boolean>
}) {
  const create = !user
  const [name, setName] = useState(user?.name ?? '')
  const [on, setOn] = useState(user?.on ?? true)
  const [mode, setMode] = useState<'generate' | 'type'>('generate')
  const [generated, setGenerated] = useState(() => generatePassword())
  const [typed, setTyped] = useState('')
  const [addPw, setAddPw] = useState(create)
  const [remove, setRemove] = useState<string[]>([])
  const initialRules = user ? [...user.keys, ...user.channels, ...user.commands].join(' ') : USER_TEMPLATES[0]!.rules.join(' ')
  const [rules, setRules] = useState(initialRules)
  const [saving, setSaving] = useState(false)

  const nameErr = create ? (userNameProblem(name) ?? (existing.includes(name) ? 'A user with this name exists' : null)) : null
  const ruleList = splitRules(rules)
  const rulesErr = rulesProblem(ruleList)
  const password = mode === 'generate' ? generated : typed
  const pwErr = addPw && mode === 'type' && typed.length > 0 && typed.length < 12 ? 'At least 12 characters' : null
  const rulesChanged = rules.trim() !== initialRules.trim()
  const left = (user?.passwords.length ?? 0) - remove.length + (addPw ? 1 : 0)

  const save = async () => {
    setSaving(true)
    const req: StoreRequest = { op: 'acl_setuser', user: name, state: on ? 'on' : 'off' }
    if (create) req.create = true
    if (addPw && password) req.addHashes = [await sha256Hex(password)]
    if (remove.length) req.removeHashes = remove
    if (create || rulesChanged) req.rules = ruleList
    await onSave(req, create ? `${name} was made.` : `${name} was changed.`)
    setSaving(false)
  }

  const disabled = !!nameErr || !!rulesErr || !!pwErr || (addPw && !password) || (create && !name)
  return (
    <Drawer opened onClose={onClose} position="right" size="lg" title={create ? 'New Redis user' : `Edit ${user!.name}`}>
      <Stack gap="md">
        {create && <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value.trim())} error={name ? nameErr : undefined} styles={{ input: { fontFamily: 'var(--font-mono)' } }} placeholder="checkout-api" />}
        <Switch label={on ? 'On: can sign in' : 'Off: nobody can sign in as it; signed-in clients stay'} checked={on} onChange={(e) => setOn(e.currentTarget.checked)} />

        <Stack gap={SP.cozy}>
          <Text size={FS.meta} fw={600}>Passwords</Text>
          {user && user.passwords.length > 0 && (
            <Table verticalSpacing={2} fz="xs" withTableBorder>
              <Table.Tbody>
                {user.passwords.map((p) => {
                  const gone = remove.includes(p)
                  return (
                    <Table.Tr key={p}>
                      <Table.Td ff="monospace" td={gone ? 'line-through' : undefined}>sha256 {p}</Table.Td>
                      <Table.Td w={90}>
                        <Button size="compact-xs" variant="subtle" color={gone ? undefined : 'rose'}
                          onClick={() => setRemove((r) => (gone ? r.filter((x) => x !== p) : [...r, p]))}>{gone ? 'Keep' : 'Remove'}</Button>
                      </Table.Td>
                    </Table.Tr>
                  )
                })}
              </Table.Tbody>
            </Table>
          )}
          {user?.nopass && <Text size={FS.micro} c="rose">This user needs no password now; adding one makes it need one.</Text>}
          {!create && !addPw && <Group><Button size="compact-xs" variant="default" leftSection={<IconPlus size={12} />} onClick={() => setAddPw(true)}>Add a password</Button></Group>}
          {addPw && (
            <Stack gap={6}>
              <SegmentedControl size="xs" value={mode} onChange={(v) => setMode(v as 'generate' | 'type')} data={[{ value: 'generate', label: 'Generate' }, { value: 'type', label: 'Type one' }]} />
              {mode === 'generate' ? (
                <Group gap={6} wrap="nowrap">
                  <Code style={{ flex: 1, fontSize: 13 }}>{generated}</Code>
                  <CopyButton value={generated}>
                    {({ copied, copy }) => (
                      <Tooltip label={copied ? 'Copied' : 'Copy'}><ActionIcon variant="default" onClick={copy} aria-label="Copy password">{copied ? <IconCheck size={14} /> : <IconCopy size={14} />}</ActionIcon></Tooltip>
                    )}
                  </CopyButton>
                  <Button size="compact-xs" variant="subtle" onClick={() => setGenerated(generatePassword())}>New</Button>
                </Group>
              ) : (
                <PasswordInput value={typed} onChange={(e) => setTyped(e.currentTarget.value)} error={pwErr} aria-label="Password" />
              )}
              <Text size={FS.micro} c="dimmed">Copy it now: only its SHA-256 is sent, so it cannot be shown again. Clients keep working while both old and new passwords are set.</Text>
              {!create && <Group><Button size="compact-xs" variant="subtle" leftSection={<IconX size={12} />} onClick={() => setAddPw(false)}>Don't add one</Button></Group>}
            </Stack>
          )}
          {!create && on && left === 0 && !user?.nopass && <Text size={FS.micro} c="amber">With no password left, nobody can sign in as {user!.name}.</Text>}
        </Stack>

        <Stack gap={SP.cozy}>
          <Group justify="space-between">
            <Text size={FS.meta} fw={600}>What it can do</Text>
            <Select size="xs" placeholder="Start from…" w={200} data={USER_TEMPLATES.map((t) => ({ value: t.id, label: t.label }))}
              onChange={(id) => { const t = USER_TEMPLATES.find((x) => x.id === id); if (t) setRules(t.rules.join(' ')) }} aria-label="Rule template" />
          </Group>
          <Textarea value={rules} onChange={(e) => setRules(e.currentTarget.value)} autosize minRows={2} error={rulesErr ?? undefined}
            styles={{ input: { fontFamily: 'var(--font-mono)', fontSize: 12.5 } }} aria-label="ACL rules"
            description="Key patterns (~cart:* or %R~cache:*), channels (&orders:*), then commands and categories (+@read -@dangerous). Replaces what it has." />
          <RulesWords rules={ruleList} />
          <CheckCommand s={s} rules={ruleList} />
        </Stack>

        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button onClick={() => void save()} loading={saving} disabled={disabled}>{create ? 'Make user' : 'Save'}</Button>
        </Group>
      </Stack>
    </Drawer>
  )
}

function RulesWords({ rules }: { rules: string[] }) {
  const keys = rules.filter((r) => ruleKind(r) === 'key')
  const chans = rules.filter((r) => ruleKind(r) === 'channel')
  const cmds = rules.filter((r) => ruleKind(r) === 'command')
  return (
    <Stack gap={2}>
      <Text size={FS.micro} c="dimmed">Keys: {keys.length ? keys.map(keyRuleWords).join(' · ') : 'none'}</Text>
      <Text size={FS.micro} c="dimmed">Channels: {chans.length ? chans.map((c) => c.replace(/^&/, '')).join(' · ') : 'none'}</Text>
      <Text size={FS.micro} c="dimmed">Commands: {cmds.length ? cmds.join(' ') : 'none'}</Text>
    </Stack>
  )
}

/** Whether rules allow a command, with ACL DRYRUN on a throwaway user. */
function CheckCommand({ s, rules, initial = 'GET cart:1' }: { s: StoreSession; rules: string[]; initial?: string }) {
  const [cmd, setCmd] = useState(initial)
  const [res, setRes] = useState<{ ok: boolean; text: string } | null>(null)
  const [busy, setBusy] = useState(false)
  useEffect(() => setRes(null), [rules.join(' ')])
  const check = async () => {
    setBusy(true)
    const a = await s.ask({ op: 'acl_dryrun', rules, command: cmd })
    setBusy(false)
    const d = dataOf<RedisDryRun>(a)
    setRes(d ? { ok: d.allowed, text: d.allowed ? 'Allowed' : `Refused: ${d.reason ?? ''}` } : { ok: false, text: a.error ?? 'could not check' })
  }
  return (
    <Stack gap={4}>
      <Group gap={6} wrap="nowrap">
        <TextInput size="xs" style={{ flex: 1 }} value={cmd} onChange={(e) => setCmd(e.currentTarget.value)} aria-label="Command to check"
          styles={{ input: { fontFamily: 'var(--font-mono)' } }} onKeyDown={(e) => { if (e.key === 'Enter') void check() }} />
        <Button size="xs" variant="default" onClick={() => void check()} loading={busy} disabled={!cmd.trim() || rules.length === 0}>Check</Button>
      </Group>
      {res && <Text size={FS.micro} c={res.ok ? 'teal' : 'rose'}>{res.text}</Text>}
    </Stack>
  )
}

function LevelCheck({ s, database }: { s: StoreSession; database: Database }) {
  const [level, setLevel] = useState<DBLevel>('read')
  const rules = levelRules(database, level)
  return (
    <SectionCard title="Check a level" description="What each Argus level's Redis user may run, set in Settings → Permissions">
      <Stack gap={SP.cozy} px="md" pb="md">
        <SegmentedControl size="xs" value={level} onChange={(v) => setLevel(v as DBLevel)}
          data={[{ value: 'read', label: 'Read' }, { value: 'write', label: 'Write' }, { value: 'owner', label: 'Owner' }, { value: 'dba', label: 'DBA' }]} />
        <Code>{rules.join(' ')}</Code>
        <CheckCommand s={s} rules={rules} initial="FLUSHALL" />
      </Stack>
    </SectionCard>
  )
}

function DeleteUser({ user, onClose, onDelete }: { user: RedisUser | null; onClose: () => void; onDelete: (u: RedisUser) => Promise<void> }) {
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => setTyped(''), [user])
  return (
    <Modal opened={user !== null} onClose={onClose} title={user ? `Delete ${user.name}` : ''} size="sm">
      {user && (
        <Stack gap="sm">
          <Text size={FS.meta}>Its passwords stop working at once{user.clients ? ` and the ${user.clients} client${user.clients === 1 ? '' : 's'} signed in as it are disconnected` : ''}. This cannot be undone.</Text>
          <TextInput label={`Type ${user.name} to confirm`} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} />
          <Group justify="flex-end">
            <Button variant="default" size="xs" onClick={onClose}>Cancel</Button>
            <Button color="rose" size="xs" disabled={typed !== user.name} loading={busy}
              onClick={async () => { setBusy(true); await onDelete(user); setBusy(false) }}>Delete user</Button>
          </Group>
        </Stack>
      )}
    </Modal>
  )
}

