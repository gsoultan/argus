import {
  ActionIcon, Alert, Badge, Box, Button, Card, Checkbox, Code, CopyButton, Drawer, Group, Modal, NumberInput,
  PasswordInput, SegmentedControl, Select, SimpleGrid, Stack, Table, Text, TextInput, Tooltip, UnstyledButton,
} from '@mantine/core'
import { useQuery } from '@tanstack/react-query'
import { IconAlertTriangle, IconCheck, IconCopy, IconPlus, IconSearch, IconUserPlus, IconX } from '@tabler/icons-react'
import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react'
import { dataOf, useRedisChange, type ChangeAnswer } from '~/components/redis/useRedisChange'
import type { StoreRequest, StoreSession } from '~/components/useStoreSession'
import { get, isConfigured } from '~/lib/live'
import {
  ACL_OPERATIONS, MB, aclKey, aclLine, appBootstrap, clientSettings, coveredTopics, kafkaUserFindings, kafkaUserNameProblem,
  madeBy, mbps, planACLs, quotaWords, signsInWith, topicProblem, userCaps, userGroups,
  type BrokerUserEvent, type KafkaACL, type KafkaLoginResult, type KafkaNetworkLite, type KafkaQuotas, type KafkaUser,
  type KafkaUserResult, type KafkaUsersData, type UserPlan,
} from '~/lib/kafkaUsers'
import { generatePassword } from '~/lib/password'
import { scramCredential, type ScramMechanism } from '~/lib/scram'
import { FS, SP } from '~/theme'
import type { Database } from '~/types/domain'

/*
The users that applications sign in to Kafka with, inside this entry's scope:
what each can do here in words, who made it, and, at Admin, making one,
changing what it can do, its limits and its password, taking away its access
here, and deleting it where everything it has is in view.

The password is made or typed here and only its SCRAM salted hash leaves the
browser; the one exception is the sign-in check right after, which the agent
uses once and drops. Argus does not keep it, so it is shown once.
*/

const MONO = { input: { fontFamily: 'var(--font-mono)' } }

const CAP_COLOR: Record<string, string> = { Produce: 'indigo', Consume: 'teal', 'Alter topics': 'amber', Deny: 'rose' }

function Caps({ acls }: { acls: KafkaACL[] }) {
  const caps = userCaps(acls)
  if (caps.length === 0) return <Text size={FS.micro} c="dimmed">No topic access here</Text>
  return (
    <Stack gap={2}>
      {caps.map((c) => (
        <Group key={c.cap} gap={6} wrap="nowrap">
          <Badge size="xs" variant="light" color={CAP_COLOR[c.cap]} radius="sm" style={{ textTransform: 'none', flexShrink: 0 }}>{c.cap}</Badge>
          <Text size={FS.micro} ff="monospace" truncate>{c.topics.join(', ')}</Text>
        </Group>
      ))}
    </Stack>
  )
}

function useUserEvents(database: Database) {
  return useQuery({
    queryKey: ['broker-users', database.id],
    queryFn: () => get<{ events: BrokerUserEvent[] }>(`/api/v1/databases/${encodeURIComponent(database.id)}/broker-users`),
    enabled: isConfigured(),
    staleTime: 10_000,
  })
}

export function KafkaUsers({ s, database, topics, can, narrow }: {
  s: StoreSession
  database: Database
  topics: string[]
  can: (op: string) => boolean
  narrow: boolean
}) {
  const scope = database.dbname
  const { change, dialog } = useRedisChange(s, database)
  const events = useUserEvents(database)
  const [data, setData] = useState<KafkaUsersData | null>(null)
  const [err, setErr] = useState<string | null>(null)
  const [q, setQ] = useState('')
  const [creating, setCreating] = useState(false)
  const [open, setOpen] = useState<string | null>(null)
  const [made, setMade] = useState<{ user: string; password: string; mechanism: ScramMechanism; group?: string } | null>(null)
  const [showSessions, setShowSessions] = useState(false)
  const manage = can('user_set')

  const load = useCallback(async () => {
    const a = await s.ask({ op: 'users' })
    const d = dataOf<KafkaUsersData>(a)
    if (!d) setErr(a.error ?? 'could not list the users')
    else { setErr(null); setData(d) }
    void events.refetch()
  }, [s]) // eslint-disable-line react-hooks/exhaustive-deps
  useEffect(() => { if (s.status === 'ready') void load() }, [s.status, load])

  const madeMap = useMemo(() => madeBy(events.data?.events ?? []), [events.data])
  const rows = useMemo(() => {
    const needle = q.trim().toLowerCase()
    return (data?.users ?? []).filter((u) => !needle || u.name.toLowerCase().includes(needle))
  }, [data, q])
  const findings = useMemo(() => kafkaUserFindings(data?.users ?? [], scope), [data, scope])

  if (!data) return err ? <Alert color="rose" variant="light">{err}</Alert> : <Text size={FS.meta} c="dimmed">Reading the users…</Text>
  const user = open ? data.users.find((u) => u.name === open) ?? null : null
  const where = scope === '*' ? 'this cluster' : scope
  return (
    <Stack gap="md">
      {findings.length > 0 && (
        <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />} title={`${findings.length} finding${findings.length === 1 ? '' : 's'}`}>
          <Stack gap={2}>{findings.map((f) => <Text key={f.user} size={FS.meta}>{f.text}</Text>)}</Stack>
        </Alert>
      )}
      <Card withBorder radius="md" padding={0}>
        <Group p="md" gap="sm" align="flex-start" wrap={narrow ? 'wrap' : 'nowrap'} style={{ borderBottom: '1px solid var(--color-line)' }}>
          <Box style={{ flex: 1, minWidth: 220 }}>
            <Text fw={600} size={FS.body}>Users of apps on {scope === '*' ? 'this cluster' : 'this cluster, in ' + scope}</Text>
            <Text size={FS.micro} c="dimmed">
              {scope === '*' ? 'Every user on the cluster, what it can do, and who made it.' : `The users that can reach topics starting ${scope}, what they can do here, and who made them.`}
              {data.notShown > 0 && ` ${data.notShown} other user${data.notShown === 1 ? '' : 's'} on the cluster reach no topic in this entry and ${data.notShown === 1 ? 'is' : 'are'} not shown.`}
            </Text>
          </Box>
          <TextInput size="xs" placeholder="Search users" leftSection={<IconSearch size={12} />} value={q} onChange={(e) => setQ(e.currentTarget.value)}
            aria-label="Search users" w={narrow ? '100%' : 220} />
          {manage && <Button size="xs" leftSection={<IconUserPlus size={14} />} onClick={() => setCreating(true)}>New user</Button>}
        </Group>
        {narrow ? (
          <Stack gap={0}>
            {rows.map((u) => (
              <UnstyledButton key={u.name} onClick={() => setOpen(u.name)} p="sm" style={{ borderBottom: '1px solid var(--color-line)' }}>
                <Group gap={6} mb={4}><Text ff="monospace" fw={600} size={FS.meta}>{u.name}</Text>{u.self && <Badge size="xs" variant="outline" color="slate">Argus signs in as this</Badge>}</Group>
                <Caps acls={u.acls} />
                <Text size={FS.micro} c="dimmed" mt={4}>{signsInWith(u)} · {madeText(madeMap.get(u.name))}{u.outside?.length ? ` · ${u.outside.length} ACLs outside ${where}` : ''}</Text>
              </UnstyledButton>
            ))}
          </Stack>
        ) : (
          <Table.ScrollContainer minWidth={820}>
            <Table verticalSpacing={8} fz="sm" highlightOnHover>
              <Table.Thead>
                <Table.Tr>
                  <Table.Th>User</Table.Th><Table.Th>Signs in with</Table.Th><Table.Th>Can do in {where}</Table.Th>
                  <Table.Th>Consumer groups</Table.Th><Table.Th>Also outside {where}</Table.Th><Table.Th>Made</Table.Th><Table.Th />
                </Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {rows.map((u) => (
                  <Table.Tr key={u.name}>
                    <Table.Td>
                      <Text ff="monospace" fw={600} size={FS.meta}>{u.name}</Text>
                      {u.self && <Badge size="xs" variant="outline" color="slate">Argus signs in as this</Badge>}
                    </Table.Td>
                    <Table.Td><Text size={FS.micro} c="dimmed">{signsInWith(u)}</Text></Table.Td>
                    <Table.Td maw={340}><Caps acls={u.acls} /></Table.Td>
                    <Table.Td><Text size={FS.micro} ff="monospace">{userGroups(u.acls).join(', ') || '–'}</Text></Table.Td>
                    <Table.Td>{u.outside?.length ? <Badge size="sm" variant="light" color="slate" radius="sm" style={{ textTransform: 'none' }}>{u.outside.length} ACL{u.outside.length === 1 ? '' : 's'}</Badge> : <Text size={FS.micro} c="dimmed">–</Text>}</Table.Td>
                    <Table.Td><Text size={FS.micro} c={madeMap.get(u.name) ? undefined : 'dimmed'}>{madeText(madeMap.get(u.name))}</Text></Table.Td>
                    <Table.Td ta="right"><Button size="compact-xs" variant="default" onClick={() => setOpen(u.name)}>Open</Button></Table.Td>
                  </Table.Tr>
                ))}
                {rows.length === 0 && (
                  <Table.Tr><Table.Td colSpan={7}><Text size={FS.meta} c="dimmed" p="sm">{data.users.length ? 'No user matches.' : `No app user reaches ${where} yet.`}</Text></Table.Td></Table.Tr>
                )}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
        <Group gap="xs" p="sm" style={{ borderTop: '1px solid var(--color-line)' }}>
          <Checkbox size="xs" checked={showSessions} onChange={(e) => setShowSessions(e.currentTarget.checked)}
            label={`Show Argus's session users · ${data.sessions.length} now`} />
          <Text size={FS.micro} c="dimmed">Made for each person's session and removed when it closes; never changed here.</Text>
        </Group>
        {showSessions && (
          <Text size={FS.micro} ff="monospace" px="sm" pb="sm">{data.sessions.join(', ') || 'None open.'}</Text>
        )}
      </Card>

      {creating && (
        <NewKafkaUser scope={scope} topics={topics} existing={data.users.map((u) => u.name)} narrow={narrow} change={change}
          onClose={() => setCreating(false)}
          onMade={(m) => { setCreating(false); setMade(m); void load() }} />
      )}
      {made && <MadeKafkaUser s={s} database={database} made={made} onClose={() => setMade(null)} />}
      {user && (
        <KafkaUserDrawer database={database} user={user} manage={manage && !user.self} narrow={narrow} change={change}
          events={(events.data?.events ?? []).filter((e) => e.user === user.name)}
          onClose={() => setOpen(null)} onChanged={() => void load()}
          onPassword={(m) => setMade(m)} />
      )}
      {dialog}
    </Stack>
  )
}

function madeText(e?: BrokerUserEvent): string {
  if (!e) return 'Outside Argus'
  const who = e.by.split('@')[0]
  return `Argus · ${who}, ${new Date(e.at).toLocaleDateString(undefined, { day: 'numeric', month: 'short' })}`
}

/** The result of a change, as one line, or null when it did not happen. */
function outcome(a: ChangeAnswer): { ok: boolean; text: string } {
  if (a.notApproved) return { ok: false, text: 'Nothing changed: it was not approved.' }
  const r = dataOf<KafkaUserResult>(a)
  if (!r) return { ok: false, text: a.error ?? 'failed' }
  return { ok: true, text: [r.added ? `${r.added} ACL${r.added === 1 ? '' : 's'} added` : '', r.removed ? `${r.removed} removed` : ''].filter(Boolean).join(', ') }
}

/** Generate a password or type one; the same control as the Redis users. */
function PasswordChoice({ value, onChange }: { value: string; onChange: (pw: string) => void }) {
  const [mode, setMode] = useState<'generate' | 'type'>('generate')
  const [generated, setGenerated] = useState(() => generatePassword())
  const [typed, setTyped] = useState('')
  useEffect(() => { onChange(mode === 'generate' ? generated : typed) }, [mode, generated, typed]) // eslint-disable-line react-hooks/exhaustive-deps
  const short = mode === 'type' && typed.length > 0 && typed.length < 12
  return (
    <Stack gap={4}>
      <Group gap="xs" wrap="nowrap" align="center">
        <SegmentedControl size="xs" value={mode} onChange={(v) => setMode(v as 'generate' | 'type')} w={190}
          data={[{ value: 'generate', label: 'Generate' }, { value: 'type', label: 'Type one' }]} />
        {mode === 'generate' ? (
          <Group gap={6} wrap="nowrap" style={{ flex: 1, minWidth: 0 }}>
            <Code style={{ flex: 1, fontSize: 13, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{generated}</Code>
            <CopyButton value={generated}>
              {({ copied, copy }) => <Tooltip label={copied ? 'Copied' : 'Copy'}><ActionIcon variant="default" onClick={copy} aria-label="Copy password">{copied ? <IconCheck size={14} /> : <IconCopy size={14} />}</ActionIcon></Tooltip>}
            </CopyButton>
            <Button size="compact-xs" variant="subtle" onClick={() => setGenerated(generatePassword())}>New</Button>
          </Group>
        ) : (
          <PasswordInput size="xs" style={{ flex: 1 }} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} aria-label="Password" error={short ? 'At least 12 characters' : undefined} />
        )}
      </Group>
      <Text size={FS.micro} c="dimmed">Shown again with the connection details once it is made. Argus does not keep it{value ? '' : ''}.</Text>
    </Stack>
  )
}

const WHAT: { value: UserPlan['what']; label: string; hint: string }[] = [
  { value: 'produce', label: 'Produce', hint: 'Write to topics' },
  { value: 'consume', label: 'Consume', hint: 'Read, with a group' },
  { value: 'both', label: 'Produce and consume', hint: 'Both, same topics' },
  { value: 'custom', label: 'Custom', hint: 'Pick ACLs one by one' },
]

/** Cards of equal height whatever their text, one chosen. */
export function ChoiceCards<T extends string>({ value, onChange, options, cols }: {
  value: T; onChange: (v: T) => void; options: { value: T; label: string; hint: string }[]; cols?: number
}) {
  return (
    <SimpleGrid cols={{ base: 2, sm: cols ?? options.length }} spacing={6} role="radiogroup">
      {options.map((o) => {
        const on = o.value === value
        return (
          <UnstyledButton key={o.value} role="radio" aria-checked={on} onClick={() => onChange(o.value)} p={8}
            style={{ height: '100%', borderRadius: 9, border: `1px solid ${on ? 'var(--mantine-color-azure-6)' : 'var(--color-line)'}`,
              background: on ? 'var(--mantine-color-azure-light)' : undefined, boxShadow: on ? '0 0 0 1px var(--mantine-color-azure-6) inset' : undefined }}>
            <Text size={FS.meta} fw={600}>{o.label}</Text>
            <Text size={FS.micro} c="dimmed">{o.hint}</Text>
          </UnstyledButton>
        )
      })}
    </SimpleGrid>
  )
}

function quotaFields(q: { produce: number | string; consume: number | string; request?: number | string }): KafkaQuotas {
  return {
    produceBytes: Number(q.produce) > 0 ? Math.round(Number(q.produce) * MB) : 0,
    consumeBytes: Number(q.consume) > 0 ? Math.round(Number(q.consume) * MB) : 0,
    requestPct: Number(q.request ?? 0) > 0 ? Number(q.request) : 0,
  }
}

function NewKafkaUser({ scope, topics, existing, narrow, change, onClose, onMade }: {
  scope: string; topics: string[]; existing: string[]; narrow: boolean
  change: (r: StoreRequest) => Promise<ChangeAnswer>
  onClose: () => void
  onMade: (m: { user: string; password: string; mechanism: ScramMechanism; group?: string }) => void
}) {
  const prefix = scope === '*' ? '' : scope
  const [name, setName] = useState('')
  const [mechanism, setMechanism] = useState<ScramMechanism>('SCRAM-SHA-512')
  const [password, setPassword] = useState('')
  const [plan, setPlan] = useState<UserPlan>({ what: 'both', match: 'prefix', topic: prefix, groupPrefix: '', transactions: false, custom: [] })
  const [groupTouched, setGroupTouched] = useState(false)
  const [limits, setLimits] = useState<{ produce: number | string; consume: number | string }>({ produce: '', consume: '' })
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState<string | null>(null)

  useEffect(() => { if (!groupTouched) setPlan((p) => ({ ...p, groupPrefix: name })) }, [name, groupTouched])
  const nameErr = name ? kafkaUserNameProblem(name, existing) : null
  const topicErr = plan.what === 'custom' ? null : topicProblem(scope, plan.match, plan.topic)
  const acls = planACLs(plan, name || '<name>')
  const covered = plan.what === 'custom' ? [] : coveredTopics(plan.match, plan.topic, topics)
  const quotas = quotaFields(limits)
  const anyLimit = !!(quotas.produceBytes || quotas.consumeBytes)
  const ok = !!name && !nameErr && !topicErr && password.length >= 12 && acls.length > 0

  const create = async () => {
    setBusy(true); setErr(null)
    const credential = await scramCredential(password, mechanism)
    const a = await change({ op: 'user_set', brokerUser: { name, create: true, mechanism, credential, acls, ...(anyLimit ? { quotas } : {}) } })
    setBusy(false)
    const o = outcome(a)
    if (!o.ok) { setErr(o.text); return }
    onMade({ user: name, password, mechanism, group: plan.what !== 'produce' && plan.groupPrefix ? plan.groupPrefix : undefined })
  }

  const preview = [
    '# through the agent, as Kafka\'s admin',
    `AlterUserSCRAMs upsert ${name || '<name>'}`,
    `  ${mechanism}, 8192 iterations`,
    '  salted password [redacted]',
    `CreateACLs User:${name || '<name>'} ALLOW`,
    ...groupACLs(acls),
    ...(anyLimit ? [`AlterClientQuotas user ${name || '<name>'}`, `  ${quotaWords(quotas)}`] : []),
  ].join('\n')

  return (
    <Modal opened onClose={onClose} title={<Text fw={650}>New Kafka user</Text>} size={narrow ? '100%' : 1000} fullScreen={narrow}>
      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="lg" style={{ gridTemplateColumns: narrow ? undefined : 'minmax(0,1fr) 340px' }}>
        <Stack gap="sm">
          <SimpleGrid cols={2} spacing="sm">
            <TextInput label="Name" value={name} onChange={(e) => setName(e.currentTarget.value.trim())} styles={MONO} placeholder="invoices-app" data-autofocus error={nameErr ?? undefined} />
            <Select label="Signs in with" value={mechanism} onChange={(v) => v && setMechanism(v as ScramMechanism)} allowDeselect={false}
              data={['SCRAM-SHA-512', 'SCRAM-SHA-256']} />
          </SimpleGrid>
          <Text size={FS.micro} c="dimmed" mt={-6}>Letters, digits, . _ -. Not argus-…, not the agent's admin user, not a user already on the cluster.</Text>
          <Stack gap={4}>
            <Text size={FS.meta} fw={500}>Password</Text>
            <PasswordChoice value={password} onChange={setPassword} />
          </Stack>
          <Stack gap={6}>
            <Text size={FS.meta} fw={500}>What it does</Text>
            <ChoiceCards value={plan.what} onChange={(what) => setPlan((p) => ({ ...p, what }))} options={WHAT} />
            <Box p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 10, background: 'var(--mantine-color-default-hover)' }}>
              {plan.what === 'custom' ? (
                <ACLEditor scope={scope} acls={plan.custom} onChange={(custom) => setPlan((p) => ({ ...p, custom }))} />
              ) : (
                <Stack gap="xs">
                  <SimpleGrid cols={2} spacing="sm">
                    <Stack gap={4}>
                      <Text size={FS.meta} fw={500}>Topics</Text>
                      <SegmentedControl size="xs" fullWidth value={plan.match} onChange={(v) => setPlan((p) => ({ ...p, match: v as 'prefix' | 'exact' }))}
                        data={[{ value: 'prefix', label: 'Starting with' }, { value: 'exact', label: 'Exactly' }]} />
                    </Stack>
                    <TextInput label=" " value={plan.topic} onChange={(e) => setPlan((p) => ({ ...p, topic: e.currentTarget.value.trim() }))} styles={MONO}
                      aria-label="Topic or prefix" error={plan.topic !== prefix ? topicErr ?? undefined : undefined} />
                  </SimpleGrid>
                  <Text size={FS.micro} c="dimmed">
                    {scope === '*' ? 'Any topic on the cluster.' : `Inside ${scope}, this entry's scope.`} Covers {covered.length} topic{covered.length === 1 ? '' : 's'} now{covered.length ? `: ${covered.slice(0, 4).join(', ')}${covered.length > 4 ? ` and ${covered.length - 4} more` : ''}` : ''}.
                  </Text>
                  <SimpleGrid cols={2} spacing="sm">
                    <TextInput label="Consumer groups starting with" value={plan.groupPrefix} styles={MONO} disabled={plan.what === 'produce'}
                      onChange={(e) => { setGroupTouched(true); const v = e.currentTarget.value.trim(); setPlan((p) => ({ ...p, groupPrefix: v })) }} />
                    <Stack gap={4} justify="flex-end">
                      <Checkbox label="Uses transactions" checked={plan.transactions} disabled={plan.what === 'consume'} mb={8}
                        onChange={(e) => { const v = e.currentTarget.checked; setPlan((p) => ({ ...p, transactions: v })) }} />
                    </Stack>
                  </SimpleGrid>
                  <Text size={FS.micro} c="dimmed">Groups starting with its name keep it from committing offsets for another app's group. Transactions add transactional IDs starting with its name.</Text>
                </Stack>
              )}
            </Box>
          </Stack>
          <Stack gap={4}>
            <Text size={FS.meta} fw={500}>Limits <Text span size={FS.micro} c="dimmed">· optional, Kafka quotas for this user</Text></Text>
            <SimpleGrid cols={2} spacing="sm">
              <NumberInput size="sm" placeholder="No limit" rightSection={<Text size={FS.micro} c="dimmed" pr={6}>MB/s produced</Text>} rightSectionWidth={110}
                value={limits.produce} min={0} decimalScale={2} onChange={(v) => setLimits((l) => ({ ...l, produce: v }))} aria-label="MB/s produced" />
              <NumberInput size="sm" placeholder="No limit" rightSection={<Text size={FS.micro} c="dimmed" pr={6}>MB/s consumed</Text>} rightSectionWidth={110}
                value={limits.consume} min={0} decimalScale={2} onChange={(v) => setLimits((l) => ({ ...l, consume: v }))} aria-label="MB/s consumed" />
            </SimpleGrid>
          </Stack>
        </Stack>
        <Stack gap="xs" p="md" style={{ background: 'var(--mantine-color-default-hover)', borderRadius: 10 }}>
          <Text size={FS.meta} fw={600}>Runs on the cluster</Text>
          <Code block style={{ fontSize: 11.5, whiteSpace: 'pre-wrap' }}>{preview}</Code>
          <Text size={FS.meta} fw={600} mt="xs">Then</Text>
          <Text size={FS.micro}>• {name || 'The user'} signs in on the entry's listener with the new password and lists what it sees.</Text>
          <Text size={FS.micro}>• Connection details, shown once.</Text>
          <Alert color="sky" variant="light" p="xs" mt="xs">
            <Text size={FS.micro}>The password is salted and hashed in your browser. Kafka gets only the salted hash; the agent and Argus never see the password.</Text>
          </Alert>
        </Stack>
      </SimpleGrid>
      {err && <Alert color="rose" variant="light" mt="sm">{err}</Alert>}
      <Group mt="md" gap="xs">
        <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>Recorded in the session and in Activity as broker.user.created.</Text>
        <Button variant="default" onClick={onClose}>Cancel</Button>
        <Button onClick={() => void create()} loading={busy} disabled={!ok}>Create {name || 'user'}</Button>
      </Group>
    </Modal>
  )
}

/** ACLs grouped for the preview: one heading per resource and name, the operations after it. */
function groupACLs(acls: KafkaACL[]): string[] {
  const by = new Map<string, string[]>()
  for (const a of acls) {
    const k = `  ${a.resource.replace('transactional_id', 'txn id')}  ${a.pattern} ${a.name}${a.permission === 'DENY' ? ' (deny)' : ''}`
    by.set(k, [...(by.get(k) ?? []), a.operation])
  }
  return [...by.entries()].flatMap(([k, ops]) => [k, `    ${ops.join(', ')}`])
}

/** A list of ACL rows, each removable, and a row to add one. Allow only: DENY rules are kept, never made. */
function ACLEditor({ scope, acls, onChange, locked }: { scope: string; acls: KafkaACL[]; onChange: (a: KafkaACL[]) => void; locked?: Set<string> }) {
  const prefix = scope === '*' ? '' : scope
  const [row, setRow] = useState<KafkaACL>({ resource: 'topic', pattern: 'PREFIXED', name: prefix, operation: 'READ', permission: 'ALLOW', host: '*' })
  const problem = row.resource === 'topic' ? topicProblem(scope, row.pattern === 'PREFIXED' ? 'prefix' : 'exact', row.name) : (row.name ? null : 'Name it')
  const dup = acls.some((a) => aclKey(a) === aclKey(row))
  return (
    <Stack gap={6}>
      {acls.length === 0 && <Text size={FS.micro} c="dimmed">No ACLs here yet.</Text>}
      {acls.map((a) => (
        <Group key={aclKey(a)} gap={6} wrap="nowrap">
          <Badge size="xs" variant="light" color={a.permission === 'DENY' ? 'rose' : 'teal'} radius="sm">{a.permission === 'DENY' ? 'Deny' : 'Allow'}</Badge>
          <Text size={FS.micro} ff="monospace" style={{ flex: 1 }} truncate>{a.resource} {a.pattern} {a.name} · {a.operation}{a.host !== '*' ? ` from ${a.host}` : ''}</Text>
          {!locked?.has(aclKey(a)) && (
            <ActionIcon size="sm" variant="subtle" color="slate" aria-label={`Remove ${aclLine(a)}`} onClick={() => onChange(acls.filter((x) => aclKey(x) !== aclKey(a)))}><IconX size={12} /></ActionIcon>
          )}
        </Group>
      ))}
      <Group gap={6} wrap="wrap" align="flex-end">
        <Select size="xs" w={130} aria-label="On" value={row.resource} allowDeselect={false}
          data={[{ value: 'topic', label: 'Topic' }, { value: 'group', label: 'Consumer group' }, { value: 'transactional_id', label: 'Transactional ID' }]}
          onChange={(v) => v && setRow((r) => ({ ...r, resource: v, operation: ACL_OPERATIONS[v]![0]!, name: v === 'topic' ? prefix : '' }))} />
        <Select size="xs" w={110} aria-label="Pattern" value={row.pattern} allowDeselect={false}
          data={[{ value: 'PREFIXED', label: 'Starting' }, { value: 'LITERAL', label: 'Exactly' }]} onChange={(v) => v && setRow((r) => ({ ...r, pattern: v }))} />
        <TextInput size="xs" style={{ flex: 1, minWidth: 120 }} aria-label="Name" styles={MONO} value={row.name} onChange={(e) => { const v = e.currentTarget.value.trim(); setRow((r) => ({ ...r, name: v })) }} />
        <Select size="xs" w={150} aria-label="Operation" value={row.operation} allowDeselect={false} data={ACL_OPERATIONS[row.resource] ?? []}
          onChange={(v) => v && setRow((r) => ({ ...r, operation: v }))} />
        <Button size="xs" variant="default" leftSection={<IconPlus size={12} />} disabled={!!problem || dup} onClick={() => onChange([...acls, row])}>Add</Button>
      </Group>
      {row.name && problem && <Text size={FS.micro} c="rose">{problem}</Text>}
    </Stack>
  )
}

function KafkaUserDrawer({ database, user, manage, narrow, change, events, onClose, onChanged, onPassword }: {
  database: Database; user: KafkaUser; manage: boolean; narrow: boolean
  change: (r: StoreRequest) => Promise<ChangeAnswer>
  events: BrokerUserEvent[]
  onClose: () => void; onChanged: () => void
  onPassword: (m: { user: string; password: string; mechanism: ScramMechanism }) => void
}) {
  const scope = database.dbname
  const where = scope === '*' ? 'this cluster' : scope
  const [acls, setAcls] = useState<KafkaACL[]>(user.acls)
  const [limits, setLimits] = useState({ produce: mbps(user.quotas.produceBytes) as number | string, consume: mbps(user.quotas.consumeBytes) as number | string, request: (user.quotas.requestPct || '') as number | string })
  const [note, setNote] = useState<{ ok: boolean; text: string } | null>(null)
  const [busy, setBusy] = useState<string | null>(null)
  const [renewing, setRenewing] = useState(false)
  const [removing, setRemoving] = useState<'scope' | 'user' | null>(null)
  useEffect(() => { setAcls(user.acls) }, [user])
  const hidden = (user.outside?.length ?? 0) > 0
  const denies = useMemo(() => new Set<string>(), [])
  const aclsChanged = acls.map(aclKey).sort().join('|') !== user.acls.map(aclKey).sort().join('|')
  const quotas = quotaFields(limits)
  const quotasChanged = (quotas.produceBytes ?? 0) !== (user.quotas.produceBytes ?? 0) || (quotas.consumeBytes ?? 0) !== (user.quotas.consumeBytes ?? 0) || (quotas.requestPct ?? 0) !== (user.quotas.requestPct ?? 0)

  const run = async (what: string, req: StoreRequest): Promise<boolean> => {
    setBusy(what); setNote(null)
    const a = await change(req)
    setBusy(null)
    const o = outcome(a)
    setNote({ ok: o.ok, text: o.ok ? `${what}${o.text ? `: ${o.text}` : ''}.` : o.text })
    if (o.ok) onChanged()
    return o.ok
  }
  const save = () => void run('Saved', { op: 'user_set', brokerUser: { name: user.name, ...(aclsChanged ? { acls } : {}), ...(quotasChanged && !hidden ? { quotas } : {}) } })

  return (
    <Drawer opened onClose={onClose} position="right" size={narrow ? '100%' : 600}
      title={<Group gap={8}><Text ff="monospace" fw={650}>{user.name}</Text>{user.self && <Badge size="sm" variant="outline" color="slate">Argus signs in as this</Badge>}</Group>}>
      <Stack gap="md">
        <Text size={FS.micro} c="dimmed" mt={-8}>Signs in with {signsInWith(user)} · {events.find((e) => e.action === 'broker.user.created') ? `made in Argus by ${events.find((e) => e.action === 'broker.user.created')!.by}` : 'made outside Argus'}</Text>
        {note && <Alert color={note.ok ? 'teal' : 'rose'} variant="light" withCloseButton onClose={() => setNote(null)}>{note.text}</Alert>}
        <Section title={`What it can do in ${where}`}>
          {manage ? <ACLEditor scope={scope} acls={acls} onChange={setAcls} locked={denies} /> : (
            <Stack gap={2}>{user.acls.map((a) => <Text key={aclKey(a)} size={FS.micro} ff="monospace">{aclLine(a)}</Text>)}{user.acls.length === 0 && <Text size={FS.micro} c="dimmed">None here.</Text>}</Stack>
          )}
          {hidden && (
            <Box p="xs" mt={6} style={{ border: '1px dashed var(--color-line)', borderRadius: 8 }}>
              <Text size={FS.micro}>Also on the cluster: <b>{user.outside!.length} ACL{user.outside!.length === 1 ? '' : 's'}</b> outside {where} ({user.outside!.slice(0, 3).map(aclLine).join('; ')}{user.outside!.length > 3 ? '; …' : ''}). Changed from a * entry.</Text>
            </Box>
          )}
        </Section>
        <Section title="Limits" hint="Kafka quotas, for this user everywhere on the cluster">
          <SimpleGrid cols={3} spacing="xs">
            <NumberInput size="xs" label="Produce at most" placeholder="No limit" value={limits.produce} min={0} decimalScale={2} disabled={!manage || hidden}
              rightSection={<Text size={FS.micro} c="dimmed" pr={6}>MB/s</Text>} onChange={(v) => setLimits((l) => ({ ...l, produce: v }))} />
            <NumberInput size="xs" label="Consume at most" placeholder="No limit" value={limits.consume} min={0} decimalScale={2} disabled={!manage || hidden}
              rightSection={<Text size={FS.micro} c="dimmed" pr={6}>MB/s</Text>} onChange={(v) => setLimits((l) => ({ ...l, consume: v }))} />
            <NumberInput size="xs" label="Request time" placeholder="No limit" value={limits.request} min={0} max={10000} disabled={!manage || hidden}
              rightSection={<Text size={FS.micro} c="dimmed" pr={6}>%</Text>} onChange={(v) => setLimits((l) => ({ ...l, request: v }))} />
          </SimpleGrid>
          {hidden && <Text size={FS.micro} c="dimmed" mt={4}>Limits and the password change from a * entry, where all it has is in view.</Text>}
        </Section>
        {manage && !hidden && user.mechanisms?.length ? (
          <Section title="Password">
            <Group gap="xs" wrap="nowrap" align="flex-start">
              <Button size="xs" variant="default" onClick={() => setRenewing(true)}>Give it a new password</Button>
              <Text size={FS.micro} c="dimmed">The app needs the new one before it reconnects; the old one stops working at once.</Text>
            </Group>
          </Section>
        ) : null}
        <Section title="Recent changes">
          {events.length === 0 ? <Text size={FS.micro} c="dimmed">None made through Argus.</Text> : (
            <Stack gap={4}>
              {events.slice(0, 8).map((e, i) => (
                <Text key={i} size={FS.micro}><Text span c="dimmed" size={FS.micro}>{new Date(e.at).toLocaleString()} · {e.by}</Text> {e.action.replace('broker.user.', '')}: {e.text.split(';')[0]}</Text>
              ))}
            </Stack>
          )}
        </Section>
        {manage && (
          <Group gap="xs" pt="sm" style={{ borderTop: '1px solid var(--color-line)' }} wrap="wrap">
            {user.acls.length > 0 && <Button size="xs" variant="outline" color="rose" onClick={() => setRemoving('scope')}>Remove its access to {where}</Button>}
            {!hidden && <Button size="xs" variant="subtle" color="rose" onClick={() => setRemoving('user')}>Delete user</Button>}
            <Box style={{ flex: 1 }} />
            <Button size="xs" onClick={save} loading={busy === 'Saved'} disabled={!aclsChanged && !(quotasChanged && !hidden)}>Save</Button>
          </Group>
        )}
        {manage && hidden && <Text size={FS.micro} c="dimmed">Deleting the user is offered only where every ACL it has is in view: a * entry.</Text>}
      </Stack>
      {renewing && (
        <RenewPassword user={user} onClose={() => setRenewing(false)} onRenew={async (password, mechanism) => {
          const credential = await scramCredential(password, mechanism)
          const ok = await run('New password set', { op: 'user_set', brokerUser: { name: user.name, mechanism, credential } })
          if (ok) { setRenewing(false); onPassword({ user: user.name, password, mechanism }) }
        }} />
      )}
      <ConfirmTyped opened={removing !== null} name={user.name}
        title={removing === 'user' ? `Delete ${user.name}` : `Remove ${user.name}'s access to ${where}`}
        text={removing === 'user'
          ? `Deletes its password, its ${user.acls.length} ACL${user.acls.length === 1 ? '' : 's'} and its limits. Apps signed in as it are refused from now on. This cannot be undone.`
          : `Deletes its ${user.acls.length} ACL${user.acls.length === 1 ? '' : 's'} in ${where}. The user and anything it has elsewhere stay.`}
        action={removing === 'user' ? 'Delete user' : 'Remove access'}
        onCancel={() => setRemoving(null)}
        onConfirm={async () => {
          const op = removing === 'user' ? 'user_del' : 'user_scope_del'
          const ok = await run(removing === 'user' ? `${user.name} was deleted` : `Removed its access to ${where}`, { op, brokerUser: { name: user.name } })
          setRemoving(null)
          if (ok && op === 'user_del') onClose()
        }} />
    </Drawer>
  )
}

function Section({ title, hint, children }: { title: string; hint?: string; children: ReactNode }) {
  return (
    <Stack gap={SP.cozy}>
      <Text size={FS.meta} fw={600}>{title}{hint && <Text span size={FS.micro} c="dimmed" fw={400}> · {hint}</Text>}</Text>
      {children}
    </Stack>
  )
}

function RenewPassword({ user, onClose, onRenew }: { user: KafkaUser; onClose: () => void; onRenew: (pw: string, m: ScramMechanism) => Promise<void> }) {
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState(false)
  const mechanism = (user.mechanisms?.[0] as ScramMechanism) || 'SCRAM-SHA-512'
  return (
    <Modal opened onClose={onClose} title={`A new password for ${user.name}`} size="lg">
      <Stack gap="sm">
        <PasswordChoice value={password} onChange={setPassword} />
        <Alert color="amber" variant="light"><Text size={FS.micro}>The old password stops working as soon as this is set. Put the new one in the app first, or be ready to.</Text></Alert>
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>Cancel</Button>
          <Button loading={busy} disabled={password.length < 12} onClick={async () => { setBusy(true); await onRenew(password, mechanism); setBusy(false) }}>Set the new password</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

function ConfirmTyped({ opened, name, title, text, action, onCancel, onConfirm }: {
  opened: boolean; name: string; title: string; text: string; action: string; onCancel: () => void; onConfirm: () => Promise<void>
}) {
  const [typed, setTyped] = useState('')
  const [busy, setBusy] = useState(false)
  useEffect(() => setTyped(''), [opened])
  return (
    <Modal opened={opened} onClose={onCancel} title={title} size="sm">
      <Stack gap="sm">
        <Text size={FS.meta}>{text}</Text>
        <TextInput label={`Type ${name} to confirm`} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} styles={MONO} />
        <Group justify="flex-end">
          <Button variant="default" size="xs" onClick={onCancel}>Cancel</Button>
          <Button color="rose" size="xs" disabled={typed !== name} loading={busy} onClick={async () => { setBusy(true); await onConfirm(); setBusy(false) }}>{action}</Button>
        </Group>
      </Stack>
    </Modal>
  )
}

/** The user was made or given a new password: check it signs in, then show the connection details once. */
function MadeKafkaUser({ s, database, made, onClose }: {
  s: StoreSession; database: Database
  made: { user: string; password: string; mechanism: ScramMechanism; group?: string }
  onClose: () => void
}) {
  const [login, setLogin] = useState<KafkaLoginResult | null>(null)
  const [net, setNet] = useState<KafkaNetworkLite | null>(null)
  const [kind, setKind] = useState<'java' | 'librdkafka' | 'env'>('java')
  const [copied, setCopied] = useState(false)
  const [leaving, setLeaving] = useState(false)
  useEffect(() => {
    let live = true
    void s.ask({ op: 'user_login', password: made.password, brokerUser: { name: made.user, mechanism: made.mechanism } }).then((a) => {
      if (!live) return
      setLogin(dataOf<KafkaLoginResult>(a) ?? { user: made.user, address: '', ok: false, detail: a.error ?? 'the check did not run' })
    })
    if (s.ready?.ops?.includes('network')) {
      void s.ask({ op: 'network' }).then((a) => { if (live) setNet(dataOf<KafkaNetworkLite>(a)) })
    }
    return () => { live = false }
  }, []) // eslint-disable-line react-hooks/exhaustive-deps
  const app = appBootstrap(net, `${database.hostname}:${database.port}`)
  const details = { bootstrap: app.address, protocol: app.protocol, mechanism: made.mechanism, user: made.user, password: made.password, group: made.group }
  const settings = clientSettings(details, kind)
  const close = () => { if (!copied && !leaving) { setLeaving(true); return } onClose() }
  const scope = database.dbname
  const rows: [string, string][] = [['Bootstrap', details.bootstrap], ['Security', details.protocol], ['Mechanism', details.mechanism], ['User', details.user], ['Password', details.password], ...(details.group ? [['Group', details.group] as [string, string]] : [])]
  return (
    <Modal opened onClose={close} title={<Text fw={650}>{made.user}: connection details</Text>} size={900} closeOnClickOutside={false}>
      <SimpleGrid cols={{ base: 1, md: 2 }} spacing="lg">
        <Stack gap="xs">
          <Step ok={login ? login.ok : null} text={login ? (login.ok ? `Signs in on ${login.address}` : `Did not sign in on ${login.address || 'the entry\'s listener'}: ${login.detail ?? ''}`) : 'Signing in as the new user…'} />
          {login?.ok && <Step ok text={`Sees ${login.topics?.length ? login.topics.join(', ') : 'no topic yet'} in ${scope === '*' ? 'the cluster' : scope}${login.elsewhere ? `, and ${login.elsewhere} other topic${login.elsewhere === 1 ? '' : 's'}` : ', and no other topic'}`} />}
          {app.loopbackOnly && net && <Alert color="amber" variant="light" p="xs"><Text size={FS.micro}>Kafka advertises only the loopback, so apps on other machines cannot use it yet. Change advertised.listeners on the server.</Text></Alert>}
          {net?.firewallActive && !net.firewallAllowAll && (
            <Alert color="amber" variant="light" p="xs"><Text size={FS.micro}>The listener accepts connections only from {net.firewallSources?.length ? net.firewallSources.join(', ') : 'this server'} on the server's firewall. If the app runs elsewhere, open it from the Network tab.</Text></Alert>
          )}
          <Text size={FS.meta} fw={600} mt="xs">Client settings</Text>
          <SegmentedControl size="xs" value={kind} onChange={(v) => setKind(v as typeof kind)} data={[{ value: 'java', label: 'Java / Spring' }, { value: 'librdkafka', label: 'librdkafka' }, { value: 'env', label: '.env' }]} />
          <Code block style={{ fontSize: 11.5, whiteSpace: 'pre-wrap', wordBreak: 'break-all' }}>{settings}</Code>
        </Stack>
        <Stack gap="xs">
          <Group><Text size={FS.meta} fw={600}>Connection details</Text><Badge color="rose" variant="light" ml="auto">shown once</Badge></Group>
          {rows.map(([k, v]) => (
            <Group key={k} gap={6} wrap="nowrap">
              <Text size={FS.micro} c="dimmed" w={80}>{k}</Text>
              <Code style={{ flex: 1, minWidth: 0, overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>{v}</Code>
              <CopyButton value={v}>{({ copied: c, copy }) => <ActionIcon variant="default" aria-label={`Copy ${k}`} onClick={() => { copy(); if (k === 'Password') setCopied(true) }}>{c ? <IconCheck size={14} /> : <IconCopy size={14} />}</ActionIcon>}</CopyButton>
            </Group>
          ))}
          <Group gap="xs" mt="xs">
            <CopyButton value={settings}>{({ copy }) => <Button size="xs" variant="default" onClick={() => { copy(); setCopied(true) }}>Copy settings</Button>}</CopyButton>
            <CopyButton value={clientSettings(details, 'env')}>{({ copy }) => <Button size="xs" variant="default" onClick={() => { copy(); setCopied(true) }}>Copy as .env</Button>}</CopyButton>
          </Group>
          <Text size={FS.micro} c="dimmed">Argus does not keep the password; if it is lost, give {made.user} a new one from its page.</Text>
        </Stack>
      </SimpleGrid>
      <Group mt="md" justify="flex-end" gap="xs">
        {leaving && !copied && <Text size={FS.micro} c="amber" style={{ flex: 1 }}>The password will not be shown again. Close anyway?</Text>}
        <Button onClick={close}>{leaving && !copied ? 'Close without copying' : 'I\'ve saved it, close'}</Button>
      </Group>
    </Modal>
  )
}

function Step({ ok, text }: { ok: boolean | null; text: string }) {
  return (
    <Group gap={8} wrap="nowrap" align="flex-start">
      <Box w={18} h={18} style={{ borderRadius: 9, flexShrink: 0, display: 'grid', placeItems: 'center', fontSize: 11, fontWeight: 700,
        background: ok === null ? undefined : ok ? 'var(--mantine-color-teal-light)' : 'var(--mantine-color-rose-light)',
        color: ok ? 'var(--mantine-color-teal-6)' : 'var(--mantine-color-rose-6)', border: ok === null ? '2px solid var(--mantine-color-azure-6)' : undefined }}>
        {ok === null ? '' : ok ? '✓' : '!'}
      </Box>
      <Text size={FS.meta}>{text}</Text>
    </Group>
  )
}
