import { useCallback, useEffect, useRef, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Box, Button, Checkbox, Code, Group, Loader, MultiSelect, PasswordInput,
  ScrollArea, SegmentedControl, Select, Switch, Table, Tabs, Text, TextInput, Tooltip,
} from '@mantine/core'
import { IconArrowLeft, IconKey, IconPlus, IconRefresh } from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { notifyOk } from '~/lib/notify'
import { usePreview } from '~/components/DatabaseExtensions'
import {
  AdminRefused, PanelFrame, type Admin, type AdminFailure, type AdminOp, type AdminPanelProps,
} from '~/components/DatabaseAdmin'

/*
SQL Server logins and access for one database, the way SSMS's Security folder
shows them: its users and roles, what each is a member of, GRANT, REVOKE and
DENY, and what a user can do in effect, asked of SQL Server as that user.

Every change goes to the agent as a fixed operation (see
internal/tunnel/mssqlaccess.go), never SQL composed here. The agent runs it as
the admin login from the server's admin file, in one transaction, recorded
before it runs. Each form shows the agent's own preview of the statements
above its button; where this database's changes wait for approval, the
session asks for it with that text. A password typed here becomes SQL
Server's salted hash inside the agent; the preview, the recording and the
approval all say [redacted].
*/

export interface MSPrincipal {
  name: string
  type: string
  auth?: string
  login?: string
  loginDisabled?: boolean
  defaultSchema?: string
  fixed?: boolean
  system?: boolean
  argus?: boolean
  memberOf?: string[]
  members?: number
  owns?: number
  created?: string
}
interface MSPrincipals { principals: MSPrincipal[]; logins: string[] }
interface MSPermission {
  state: string; permission: string; on: string; schema?: string; object?: string; column?: string; kind?: string; grantor?: string; via?: string
}
interface MSEffective { schema: string; name?: string; kind?: string; can: string[] }
interface MSPermissions {
  principal: MSPrincipal
  memberOf: { role: string; via?: string }[]
  direct: MSPermission[]
  inherited: MSPermission[]
  database?: string[]
  schemas?: MSEffective[]
  objects?: MSEffective[]
  truncated?: boolean
  unknown?: string
}
interface MSObjects { schemas: { name: string; owner?: string }[]; objects?: { name: string; kind?: string }[]; truncated?: boolean }

type On = 'database' | 'schema' | 'object'
/** The permissions each kind of securable takes, as the agent accepts them. */
export const MS_PERMISSIONS: Record<On, string[]> = {
  database: ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'EXECUTE', 'REFERENCES', 'VIEW DEFINITION', 'SHOWPLAN',
    'VIEW DATABASE STATE', 'CREATE TABLE', 'CREATE VIEW', 'CREATE PROCEDURE', 'CREATE FUNCTION', 'CREATE SCHEMA',
    'CREATE TYPE', 'ALTER', 'ALTER ANY SCHEMA', 'BACKUP DATABASE', 'CONNECT', 'CONTROL', 'TAKE OWNERSHIP'],
  schema: ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'EXECUTE', 'REFERENCES', 'VIEW DEFINITION', 'VIEW CHANGE TRACKING', 'ALTER', 'CONTROL', 'TAKE OWNERSHIP'],
  object: ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'EXECUTE', 'REFERENCES', 'VIEW DEFINITION', 'VIEW CHANGE TRACKING', 'ALTER', 'CONTROL', 'TAKE OWNERSHIP'],
}
const COMMON: Record<On, string[]> = {
  database: ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'EXECUTE', 'VIEW DEFINITION', 'SHOWPLAN', 'CREATE TABLE', 'CREATE VIEW', 'CREATE PROCEDURE'],
  schema: ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'EXECUTE', 'VIEW DEFINITION', 'ALTER'],
  object: ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'EXECUTE', 'VIEW DEFINITION'],
}

/** What each fixed database role holds, in words. */
export const FIXED_ROLES: Record<string, string> = {
  db_owner: 'everything in this database, including who may do what',
  db_securityadmin: 'manage roles and permissions (close to db_owner)',
  db_accessadmin: 'add and remove users',
  db_backupoperator: 'back the database up',
  db_ddladmin: 'create, change and drop tables, views and procedures',
  db_datawriter: 'add, change and delete rows in every table',
  db_datareader: 'read every table and view',
  db_denydatawriter: 'never change rows, whatever else it holds',
  db_denydatareader: 'never read rows, whatever else it holds',
}

const AUTH_WORDS: Record<string, string> = {
  sql: 'SQL login', windows: 'Windows', none: 'no login', database: 'contained', external: 'Entra ID', orphaned: 'orphaned',
}

/** Who a principal is, in one badge. */
export function principalKind(p: MSPrincipal): { label: string; color: string; locked?: string } {
  if (p.argus) return { label: 'Argus person', color: 'azure', locked: 'A person’s login that Argus manages. Give them a level from the database’s settings; it is disabled between their sessions.' }
  if (p.system) return { label: 'SQL Server', color: 'slate', locked: `${p.name} is SQL Server’s own.` }
  if (p.fixed) return { label: 'Fixed role', color: 'slate' }
  if (p.type === 'role') return { label: 'Role', color: 'teal' }
  if (p.type === 'application-role') return { label: 'Application role', color: 'slate', locked: 'Application roles are changed with T-SQL.' }
  if (p.type === 'windows-user' || p.type === 'windows-group') return { label: p.type === 'windows-group' ? 'Windows group' : 'Windows user', color: 'violet' }
  return { label: AUTH_WORDS[p.auth ?? ''] ?? 'User', color: p.auth === 'orphaned' ? 'amber' : 'teal' }
}

const isRole = (p: MSPrincipal) => p.type === 'role'
/** May this be changed or dropped here? */
const changeable = (p: MSPrincipal) => !p.argus && !p.system && !p.fixed && p.type !== 'application-role'

export interface MSGrantPreset { kind?: 'ms.grant' | 'ms.revoke' | 'ms.deny'; grantee?: string; on?: On; schema?: string; objects?: string[]; privileges?: string[] }

type View =
  | { mode: 'list' }
  | { mode: 'one'; name: string; tab?: string }
  | { mode: 'new-user' }
  | { mode: 'new-role' }
  | { mode: 'edit'; p: MSPrincipal }
  | { mode: 'drop'; p: MSPrincipal }
  | { mode: 'grant'; preset: MSGrantPreset; back: View }

export function MSSQLAccess(props: AdminPanelProps) {
  const { admin, narrow, onBack } = props
  const [data, setData] = useState<MSPrincipals | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [busy, setBusy] = useState(false)
  const [filter, setFilter] = useState('')
  const [view, setView] = useState<View>({ mode: 'list' })
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const refresh = useCallback(async () => {
    setBusy(true)
    const r = await admin<MSPrincipals>({ kind: 'ms.principals' })
    if (!alive.current) return
    if (r.error) setFailure(r.error)
    else { setFailure(null); setData(r.data ?? { principals: [], logins: [] }) }
    setBusy(false)
  }, [admin])
  useEffect(() => { void refresh() }, [refresh])

  const all = data?.principals ?? []
  const roles = all.filter(isRole).map((r) => r.name).filter((n) => n !== 'public')
  const grantees = all.filter((p) => !p.argus && p.name !== 'dbo' && p.name !== 'guest' && p.type !== 'application-role').map((p) => p.name)
  const done = (next: View = { mode: 'list' }) => { setView(next); void refresh() }
  const header = (
    <Group gap={6} wrap="nowrap">
      <Button size="compact-sm" variant="default" leftSection={<IconKey size={14} />}
        onClick={() => setView({ mode: 'grant', preset: {}, back: { mode: 'list' } })}>Grant…</Button>
      <Button size="compact-sm" variant="default" leftSection={<IconPlus size={14} />} onClick={() => setView({ mode: 'new-role' })}>New role</Button>
      <Button size="compact-sm" leftSection={<IconPlus size={14} />} onClick={() => setView({ mode: 'new-user' })}>New user</Button>
      <ActionIcon variant="subtle" onClick={() => void refresh()} loading={busy} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>
    </Group>
  )

  if (view.mode === 'new-user' || (view.mode === 'edit' && !isRole(view.p))) {
    const p = view.mode === 'edit' ? view.p : undefined
    return (
      <PanelFrame title={p ? `Change ${p.name}` : 'New user'} narrow={narrow} onBack={onBack}
        blurb="A user is who someone or something is in this database; a login is how it signs in to the server.">
        <UserForm admin={admin} user={p} roles={roles} logins={data?.logins ?? []}
          onCancel={() => setView(p ? { mode: 'one', name: p.name } : { mode: 'list' })}
          onDone={(name) => done({ mode: 'one', name })} />
      </PanelFrame>
    )
  }
  if (view.mode === 'new-role' || view.mode === 'edit') {
    const p = view.mode === 'edit' ? view.p : undefined
    return (
      <PanelFrame title={p ? `Rename ${p.name}` : 'New role'} narrow={narrow} onBack={onBack}
        blurb="A role gathers permissions; users and other roles that are its members hold them.">
        <RoleForm admin={admin} role={p} roles={roles}
          onCancel={() => setView(p ? { mode: 'one', name: p.name } : { mode: 'list' })}
          onDone={(name) => done(p ? { mode: 'one', name } : { mode: 'grant', preset: { grantee: name }, back: { mode: 'one', name } })} />
      </PanelFrame>
    )
  }
  if (view.mode === 'drop') {
    return (
      <PanelFrame title={`Drop ${view.p.name}`} narrow={narrow} onBack={onBack} blurb="Removing a user or role from this database.">
        <DropForm admin={admin} p={view.p} onCancel={() => setView({ mode: 'one', name: view.p.name })} onDone={() => done()} />
      </PanelFrame>
    )
  }
  if (view.mode === 'grant') {
    return (
      <PanelFrame title="Grant, revoke or deny" narrow={narrow} onBack={onBack}
        blurb="What a user or role may do in this database. Runs as SQL Server’s admin login, so it takes effect whoever owns the objects.">
        <GrantForm admin={admin} grantees={grantees} preset={view.preset} onCancel={() => setView(view.back)}
          onDone={(g) => done(g && g !== 'public' ? { mode: 'one', name: g, tab: 'can' } : view.back)} />
      </PanelFrame>
    )
  }
  if (view.mode === 'one') {
    const p = all.find((x) => x.name === view.name)
    return (
      <PanelFrame title={view.name} narrow={narrow} onBack={onBack} right={header}
        blurb="What this is, what it belongs to, and what it can do in this database.">
        <Detail key={view.name} admin={admin} name={view.name} p={p} roles={roles} tab={view.tab} narrow={narrow}
          onBack={() => setView({ mode: 'list' })} onChanged={() => void refresh()}
          onEdit={(x) => setView({ mode: 'edit', p: x })} onDrop={(x) => setView({ mode: 'drop', p: x })}
          onGrant={(preset) => setView({ mode: 'grant', preset, back: { mode: 'one', name: view.name, tab: 'can' } })} />
      </PanelFrame>
    )
  }

  const q = filter.trim().toLowerCase()
  const shown = all.filter((p) => !q || p.name.toLowerCase().includes(q) || (p.login ?? '').toLowerCase().includes(q))
  const open = (name: string) => setView({ mode: 'one', name })
  return (
    <PanelFrame title="Logins & access" narrow={narrow} onBack={onBack} right={header}
      blurb="This database’s users and roles: create and change them, grant, revoke and deny, and see what each can do.">
      <Group gap="sm" px="md" py={6} style={{ borderBottom: '1px solid var(--color-line)' }}>
        <TextInput size="xs" placeholder={`Filter ${all.length || ''} users and roles…`} value={filter}
          onChange={(e) => setFilter(e.currentTarget.value)} w={narrow ? '100%' : 260} aria-label="Filter users and roles" />
      </Group>
      {failure ? <Box p="md"><AdminRefused error={failure} who="SQL Server" /></Box> : !data ? <Loader size={16} m="md" /> : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Table verticalSpacing={4} fz={12} striped highlightOnHover>
            <Table.Thead style={{ position: 'sticky', top: 0, background: 'var(--color-raised)', zIndex: 1 }}>
              <Table.Tr>
                <Table.Th>Name</Table.Th><Table.Th>Kind</Table.Th><Table.Th>Signs in as</Table.Th>
                {!narrow && <Table.Th>Member of</Table.Th>}{!narrow && <Table.Th>Default schema</Table.Th>}
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {shown.map((p) => {
                const k = principalKind(p)
                return (
                  <Table.Tr key={p.name}>
                    <Table.Td>
                      <Text size={FS.meta} ff="monospace" fw={600} c={INK.brand} style={{ cursor: 'pointer' }} role="button" tabIndex={0}
                        onClick={() => open(p.name)} onKeyDown={(e) => { if (e.key === 'Enter') open(p.name) }}>{p.name}</Text>
                      {p.fixed && !narrow && FIXED_ROLES[p.name] && <Text size={FS.micro} c="dimmed">{FIXED_ROLES[p.name]}</Text>}
                    </Table.Td>
                    <Table.Td><Badge size="xs" variant="light" color={k.color} radius="sm">{k.label}</Badge></Table.Td>
                    <Table.Td>
                      {isRole(p) ? <Text size={FS.micro} c="dimmed">{p.members ? `${p.members} member${p.members === 1 ? '' : 's'}` : 'no members'}</Text> : (
                        <Group gap={4} wrap="nowrap">
                          <Text size={FS.meta} ff="monospace">{p.login || '—'}</Text>
                          {p.loginDisabled && <Badge size="xs" variant="outline" color="amber" radius="sm">disabled</Badge>}
                        </Group>
                      )}
                    </Table.Td>
                    {!narrow && (
                      <Table.Td>
                        <Group gap={4}>{(p.memberOf ?? []).slice(0, 4).map((g) => <Badge key={g} size="xs" variant="outline" color="slate" radius="sm" tt="none" ff="monospace">{g}</Badge>)}
                          {(p.memberOf?.length ?? 0) > 4 && <Text size={FS.micro} c="dimmed">+{p.memberOf!.length - 4}</Text>}</Group>
                      </Table.Td>
                    )}
                    {!narrow && <Table.Td><Text size={FS.meta} ff="monospace">{p.defaultSchema || ''}</Text></Table.Td>}
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
          {shown.length === 0 && <Text size={FS.meta} c="dimmed" p="md">Nothing matches.</Text>}
          <Text size={FS.micro} c="dimmed" p="md">
            People who open this database through Argus get their level from its settings, not here; their logins are disabled between sessions.
            Every change on this page runs as SQL Server’s admin login and is in this session’s recording.
          </Text>
        </ScrollArea>
      )}
    </PanelFrame>
  )
}

/* ── One user or role ───────────────────────────────────────────────────── */

function Detail({ admin, name, p, roles, tab, narrow, onBack, onChanged, onEdit, onDrop, onGrant }: {
  admin: Admin; name: string; p?: MSPrincipal; roles: string[]; tab?: string; narrow: boolean
  onBack: () => void; onChanged: () => void; onEdit: (p: MSPrincipal) => void; onDrop: (p: MSPrincipal) => void; onGrant: (p: MSGrantPreset) => void
}) {
  const [perms, setPerms] = useState<MSPermissions | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [active, setActive] = useState(tab ?? 'can')
  const load = useCallback(async () => {
    const r = await admin<MSPermissions>({ kind: 'ms.permissions', principal: { name } })
    if (r.error) setFailure(r.error)
    else { setFailure(null); setPerms(r.data ?? null) }
  }, [admin, name])
  useEffect(() => { void load() }, [load])
  const k = p ? principalKind(p) : null
  const canChange = !!p && changeable(p)
  const canGrant = !!p && !p.argus && p.name !== 'dbo' && p.name !== 'guest' && p.type !== 'application-role'

  const undo = async (x: MSPermission) => {
    const what = `${x.state === 'DENY' ? 'the deny of' : ''} ${x.permission} on ${where(x)}`.trim()
    if (!window.confirm(`Take ${what} from ${name}?`)) return
    const r = await admin({ kind: 'ms.revoke', msGrant: { grantee: name, on: x.on as On, schema: x.schema || undefined,
      objects: x.on === 'object' ? [x.object!] : undefined, privileges: [x.permission], cascade: true } })
    if (r.error) { setFailure(r.error); return }
    notifyOk('Revoked', what)
    void load()
  }

  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box px="md" pt="sm">
        <Button size="compact-xs" variant="subtle" leftSection={<IconArrowLeft size={12} />} onClick={onBack}>All users and roles</Button>
        {p && k && (
          <Group gap={6} mt={6} wrap="wrap">
            <Badge size="sm" variant="light" color={k.color} radius="sm">{k.label}</Badge>
            {p.login && <Badge size="sm" variant="outline" color="slate" radius="sm" tt="none">login {p.login}</Badge>}
            {p.loginDisabled && <Badge size="sm" variant="light" color="amber" radius="sm">login disabled</Badge>}
            {p.defaultSchema && <Badge size="sm" variant="outline" color="slate" radius="sm" tt="none">default schema {p.defaultSchema}</Badge>}
            {!!p.owns && <Badge size="sm" variant="outline" color="slate" radius="sm">owns {p.owns} schema{p.owns === 1 ? '' : 's'}</Badge>}
          </Group>
        )}
        {p?.fixed && FIXED_ROLES[p.name] && <Text size={FS.meta} c="dimmed" mt={4}>Holds {FIXED_ROLES[p.name]}.</Text>}
        {p?.auth === 'orphaned' && (
          <Alert mt="sm" color="amber" variant="light" p="xs">
            <Text size={FS.meta}>This user’s login is not on this server, usually because the database was restored from another one. It cannot sign in until it is mapped to a login again.</Text>
          </Alert>
        )}
        {k?.locked && <Alert mt="sm" color="slate" variant="light" p="xs"><Text size={FS.meta}>{k.locked}</Text></Alert>}
        {(canGrant || canChange) && (
          <Group gap="xs" mt="sm">
            {canGrant && <Button size="xs" leftSection={<IconKey size={14} />} onClick={() => onGrant({ grantee: name })}>Grant…</Button>}
            {canChange && <Button size="xs" variant="default" onClick={() => onEdit(p!)}>{isRole(p!) ? 'Rename' : 'Change user'}</Button>}
            {canChange && <Button size="xs" variant="subtle" color="rose" onClick={() => onDrop(p!)}>Drop {isRole(p!) ? 'role' : 'user'}</Button>}
          </Group>
        )}
      </Box>
      <Tabs value={active} onChange={(v) => setActive(v ?? 'can')} mt="sm" keepMounted={false}>
        <Tabs.List px="md">
          <Tabs.Tab value="can">What it can do here</Tabs.Tab>
          <Tabs.Tab value="members">Member of{perms?.memberOf.length ? ` (${perms.memberOf.length})` : ''}</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="can">
          {failure && <Box p="md"><AdminRefused error={failure} who="SQL Server" /></Box>}
          {!perms && !failure ? <Loader size={16} m="md" /> : perms && (
            <PermissionsView perms={perms} canChange={canGrant} narrow={narrow} onUndo={(x) => void undo(x)} />
          )}
        </Tabs.Panel>
        <Tabs.Panel value="members">
          {perms && p && <Membership admin={admin} p={p} perms={perms} roles={roles} canChange={canGrant}
            onChanged={() => { onChanged(); void load() }} />}
        </Tabs.Panel>
      </Tabs>
    </ScrollArea>
  )
}

function where(x: MSPermission): string {
  if (x.on === 'database') return 'the database'
  if (x.on === 'schema') return `schema ${x.schema}`
  if (x.on === 'object') return `${x.schema}.${x.object}${x.column ? ` (${x.column})` : ''}`
  return x.on
}

function StateBadge({ state }: { state: string }) {
  const deny = state === 'DENY'
  return (
    <Badge size="xs" radius="sm" variant={deny ? 'filled' : 'light'} color={deny ? 'rose' : 'azure'}>
      {deny ? 'DENY' : state === 'GRANT_WITH_GRANT_OPTION' ? 'GRANT*' : 'GRANT'}
    </Badge>
  )
}

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <Box px="md" pt="md">
      <Text size={FS.meta} fw={650} mb={6}>{title}</Text>
      {children}
    </Box>
  )
}

function Cans({ can }: { can: string[] }) {
  if (can.length === 0) return <Text size={FS.micro} c="dimmed">nothing</Text>
  return <Group gap={4} wrap="wrap">{can.map((c) => <Badge key={c} size="xs" radius="sm" variant="light" color="teal" tt="none">{c}</Badge>)}</Group>
}

function PermissionsView({ perms, canChange, narrow, onUndo }: {
  perms: MSPermissions; canChange: boolean; narrow: boolean; onUndo: (x: MSPermission) => void
}) {
  const isUser = !!perms.database
  const rows = (list: MSPermission[], own: boolean) => (
    <Table verticalSpacing={3} fz={12}>
      <Table.Tbody>
        {list.map((x, i) => (
          <Table.Tr key={i}>
            <Table.Td w={80}><StateBadge state={x.state} /></Table.Td>
            <Table.Td><Text size={FS.meta} ff="monospace">{x.permission}</Text></Table.Td>
            <Table.Td><Text size={FS.meta}>{where(x)}</Text>{x.kind && !narrow && <Text size={FS.micro} c="dimmed">{x.kind.toLowerCase().replace(/_/g, ' ')}</Text>}</Table.Td>
            {!own && <Table.Td><Text size={FS.micro} c="dimmed">through {x.via}</Text></Table.Td>}
            {own && canChange && ['database', 'schema', 'object'].includes(x.on) && !(x.on === 'database' && x.permission === 'CONNECT') && (
              <Table.Td ta="right"><Button size="compact-xs" variant="subtle" color="rose" onClick={() => onUndo(x)}>Revoke</Button></Table.Td>
            )}
          </Table.Tr>
        ))}
      </Table.Tbody>
    </Table>
  )
  return (
    <Box pb="md">
      {isUser ? (
        <>
          <Section title="On the database, in effect">
            <Cans can={perms.database ?? []} />
          </Section>
          {(perms.schemas?.length ?? 0) > 0 && (
            <Section title="On each schema, in effect">
              <Table verticalSpacing={3} fz={12}><Table.Tbody>
                {perms.schemas!.map((s) => <Table.Tr key={s.schema}><Table.Td w={160}><Text size={FS.meta} ff="monospace">{s.schema}</Text></Table.Td><Table.Td><Cans can={s.can} /></Table.Td></Table.Tr>)}
              </Table.Tbody></Table>
            </Section>
          )}
          {(perms.objects?.length ?? 0) > 0 && (
            <Section title={`On tables, views, procedures and functions, in effect${perms.truncated ? ' (the first 500)' : ''}`}>
              <Table verticalSpacing={3} fz={12}><Table.Tbody>
                {perms.objects!.map((o) => (
                  <Table.Tr key={`${o.schema}.${o.name}`}>
                    <Table.Td><Text size={FS.meta} ff="monospace">{o.schema}.{o.name}</Text>{!narrow && <Text size={FS.micro} c="dimmed">{o.kind}</Text>}</Table.Td>
                    <Table.Td><Cans can={o.can} /></Table.Td>
                  </Table.Tr>
                ))}
              </Table.Tbody></Table>
            </Section>
          )}
        </>
      ) : perms.unknown ? (
        <Box px="md" pt="md"><Text size={FS.meta} c="dimmed">{perms.unknown}</Text></Box>
      ) : null}
      <Section title="Given to it directly">
        {perms.direct.length ? rows(perms.direct, true) : <Text size={FS.micro} c="dimmed">Nothing granted or denied to it by name.</Text>}
      </Section>
      {perms.inherited.length > 0 && <Section title="Through the roles it is in, and public">{rows(perms.inherited, false)}</Section>}
      <Text size={FS.micro} c="dimmed" px="md" pt="md">
        {isUser ? 'In effect is what SQL Server answers when asked as this user: its roles, public, and any DENY, which always wins. ' : ''}
        A fixed role’s own powers are not listed as grants; they come with membership.
      </Text>
    </Box>
  )
}

function Membership({ admin, p, perms, roles, canChange, onChanged }: {
  admin: Admin; p: MSPrincipal; perms: MSPermissions; roles: string[]; canChange: boolean; onChanged: () => void
}) {
  const [add, setAdd] = useState<string | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const direct = p.memberOf ?? []
  const op: AdminOp | null = add ? { kind: 'ms.member', member: { role: add, member: p.name } } : null
  const preview = usePreview(admin, canChange ? op : null)
  const change = async (o: AdminOp, words: string) => {
    const r = await admin(o)
    if (r.error) { setFailure(r.error); return }
    setFailure(null)
    notifyOk('Changed', words)
    setAdd(null)
    onChanged()
  }
  return (
    <Box p="md">
      {perms.memberOf.length === 0 && <Text size={FS.meta} c="dimmed">Only public, which everyone is in.</Text>}
      {perms.memberOf.map((m) => (
        <Group key={m.role} gap="xs" py={3} wrap="nowrap">
          <Badge size="sm" variant="outline" color="slate" radius="sm" tt="none" ff="monospace">{m.role}</Badge>
          <Text size={FS.micro} c="dimmed" style={{ flex: 1 }}>{m.via ? `through ${m.via}` : FIXED_ROLES[m.role] ?? ''}</Text>
          {canChange && direct.includes(m.role) && !m.via && (
            <Button size="compact-xs" variant="subtle" color="rose"
              onClick={() => { if (window.confirm(`Take ${p.name} out of ${m.role}?`)) void change({ kind: 'ms.unmember', member: { role: m.role, member: p.name } }, `${p.name} left ${m.role}`) }}>
              Remove
            </Button>
          )}
        </Group>
      ))}
      {canChange && (
        <Box mt="md" maw={560}>
          <Select size="xs" label="Add to a role" placeholder="Choose a role" searchable w={320} clearable
            data={roles.filter((r) => r !== p.name && !direct.includes(r))} value={add} onChange={setAdd}
            description={add && FIXED_ROLES[add] ? `${add}: ${FIXED_ROLES[add]}` : undefined} />
          {add && (
            <>
              <Code block mt="sm" fz={12}>{preview.sql || (preview.error ? `— ${preview.error.error}` : '…')}</Code>
              <Button size="xs" mt="sm" disabled={!preview.sql} onClick={() => void change(op!, `${p.name} joined ${add}`)}>Add</Button>
            </>
          )}
        </Box>
      )}
      {failure && <Box mt="sm"><AdminRefused error={failure} who="SQL Server" /></Box>}
    </Box>
  )
}

/* ── Forms ──────────────────────────────────────────────────────────────── */

/** Statement preview, failure and buttons, the same at the foot of every form. */
function FormFoot({ sql, previewError, failure, busy, canRun, verb, danger, onRun, onCancel }: {
  sql: string; previewError?: AdminFailure; failure: AdminFailure | null; busy: boolean; canRun: boolean; verb: string; danger?: boolean
  onRun: () => void; onCancel: () => void
}) {
  return (
    <>
      <Text size={FS.micro} c="dimmed" mt="md">This runs as SQL Server’s admin login, in one transaction, and is recorded in this session. On a database whose changes wait for approval, it is asked for first.</Text>
      <Code block mt={4} fz={12} style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-word' }}>{sql || (previewError ? `— ${previewError.error}` : '…')}</Code>
      {failure && <Box mt="sm"><AdminRefused error={failure} who="SQL Server" /></Box>}
      <Group mt="md" gap="xs">
        <Button size="xs" color={danger ? 'rose' : undefined} onClick={onRun} disabled={!canRun || !sql} loading={busy}>{verb}</Button>
        <Button size="xs" variant="default" onClick={onCancel}>Cancel</Button>
      </Group>
    </>
  )
}

function useRun(admin: Admin, done: () => void) {
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const run = async (op: AdminOp | null, title: string, words: string) => {
    if (!op) return
    setBusy(true)
    const r = await admin(op)
    setBusy(false)
    if (r.error) { setFailure(r.error); return }
    notifyOk(title, words)
    done()
  }
  return { busy, failure, run }
}

function useSchemas(admin: Admin, schema?: string | null): MSObjects | null {
  const [cat, setCat] = useState<MSObjects | null>(null)
  useEffect(() => {
    let live = true
    void admin<MSObjects>({ kind: 'ms.objects', msGrant: schema ? { schema } as AdminOp['msGrant'] : undefined }).then((r) => {
      if (live && r.data) setCat(r.data)
    })
    return () => { live = false }
  }, [admin, schema])
  return cat
}

function UserForm({ admin, user, roles, logins, onCancel, onDone }: {
  admin: Admin; user?: MSPrincipal; roles: string[]; logins: string[]; onCancel: () => void; onDone: (name: string) => void
}) {
  const editing = !!user
  const [name, setName] = useState(user?.name ?? '')
  const [auth, setAuth] = useState<'sql' | 'login' | 'none'>('sql')
  const [login, setLogin] = useState<string | null>(null)
  const [password, setPassword] = useState('')
  const [schema, setSchema] = useState<string | null>(user?.defaultSchema || 'dbo')
  const [memberOf, setMemberOf] = useState<string[]>(['db_datareader'])
  const [enabled, setEnabled] = useState(!user?.loginDisabled)
  const cat = useSchemas(admin)
  const hasLogin = !!user?.login && (user.auth === 'sql' || user.auth === 'windows')
  const { busy, failure, run } = useRun(admin, () => onDone(editing ? (name.trim() || user!.name) : name.trim()))

  let op: AdminOp | null = null
  if (editing) {
    const principal: NonNullable<AdminOp['principal']> = { name: user!.name }
    if (name.trim() && name.trim() !== user!.name) principal.rename = name.trim()
    if ((schema ?? '') !== (user!.defaultSchema ?? '')) principal.defaultSchema = schema ?? ''
    if (hasLogin && user!.auth === 'sql' && password) { principal.login = user!.login; principal.password = password }
    if (hasLogin && enabled === !!user!.loginDisabled) { principal.login = user!.login; principal.enabled = enabled }
    if (Object.keys(principal).length > 1) op = { kind: 'ms.user.alter', principal }
  } else if (name.trim() && (auth !== 'sql' || password) && (auth !== 'login' || login)) {
    op = { kind: 'ms.user.create', principal: { name: name.trim(), auth, login: auth === 'login' ? login! : undefined,
      password: auth === 'sql' ? password : undefined, defaultSchema: schema ?? undefined, memberOf } }
  }
  const preview = usePreview(admin, op)

  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box p="md" maw={640}>
        <TextInput size="xs" label={editing ? 'Name' : 'User name'} value={name} onChange={(e) => setName(e.currentTarget.value)} w={320}
          description={editing ? 'Renaming keeps its login, permissions and what it owns' : undefined} />
        {!editing && (
          <>
            <Text size={FS.meta} fw={600} mt="md">Signs in with</Text>
            <SegmentedControl size="xs" mt={4} value={auth} onChange={(v) => setAuth(v as typeof auth)}
              data={[{ value: 'sql', label: 'A new SQL login' }, { value: 'login', label: 'An existing login' }, { value: 'none', label: 'No login' }]} />
            <Text size={FS.micro} c="dimmed" mt={4}>
              {auth === 'sql' ? 'Makes a login of the same name with this password, for an application to sign in with.'
                : auth === 'login' ? 'Maps a login already on the server, such as a Windows account, to a user here.'
                  : 'A user that cannot sign in: for EXECUTE AS, or to own a schema.'}
            </Text>
            {auth === 'login' && (
              <Select size="xs" mt="sm" label="Login" placeholder="Choose a login" searchable w={320} data={logins} value={login} onChange={setLogin}
                nothingFoundMessage="Every login already has a user here" />
            )}
          </>
        )}
        {(!editing ? auth === 'sql' : hasLogin && user!.auth === 'sql') && (
          <PasswordInput size="xs" mt="sm" label={editing ? `New password for login ${user!.login}` : 'Password'} w={320}
            value={password} onChange={(e) => setPassword(e.currentTarget.value)} autoComplete="new-password"
            description="Sent to the agent once and stored by SQL Server only as its hash; never recorded or shown" />
        )}
        {editing && hasLogin && (
          <Switch size="xs" mt="md" checked={enabled} onChange={(e) => setEnabled(e.currentTarget.checked)}
            label={`Login ${user!.login} can sign in`} description={!enabled ? 'Disabling also ends its open connections' : undefined} />
        )}
        <Select size="xs" mt="sm" label="Default schema" searchable w={320} clearable data={(cat?.schemas ?? []).map((s) => s.name)}
          value={schema} onChange={setSchema} description="Where its unqualified names point" />
        {!editing && (
          <MultiSelect size="xs" mt="sm" label="Member of" searchable w={480} data={roles} value={memberOf} onChange={setMemberOf}
            description={memberOf.includes('db_owner') ? 'db_owner is everything in this database: it waits for approval on every database' : 'What it may do; grant more once it is made'} />
        )}
        <FormFoot sql={preview.sql} previewError={preview.error} failure={failure} busy={busy} canRun={!!op}
          verb={editing ? 'Change' : 'Create user'} onRun={() => void run(op, editing ? 'Changed' : 'Created', name.trim())} onCancel={onCancel} />
      </Box>
    </ScrollArea>
  )
}

function RoleForm({ admin, role, roles, onCancel, onDone }: {
  admin: Admin; role?: MSPrincipal; roles: string[]; onCancel: () => void; onDone: (name: string) => void
}) {
  const [name, setName] = useState(role?.name ?? '')
  const [memberOf, setMemberOf] = useState<string[]>([])
  const n = name.trim()
  const { busy, failure, run } = useRun(admin, () => onDone(n))
  const op: AdminOp | null = !n ? null : role
    ? (n !== role.name ? { kind: 'ms.role.alter', principal: { name: role.name, rename: n } } : null)
    : { kind: 'ms.role.create', principal: { name: n, memberOf } }
  const preview = usePreview(admin, op)
  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box p="md" maw={640}>
        <TextInput size="xs" label="Role name" value={name} onChange={(e) => setName(e.currentTarget.value)} w={320} />
        {!role && (
          <MultiSelect size="xs" mt="sm" label="Itself a member of" searchable w={480} data={roles} value={memberOf} onChange={setMemberOf}
            description="Optional: its members also hold what these roles hold" />
        )}
        <FormFoot sql={preview.sql} previewError={preview.error} failure={failure} busy={busy} canRun={!!op}
          verb={role ? 'Rename' : 'Create role'} onRun={() => void run(op, role ? 'Renamed' : 'Created', n)} onCancel={onCancel} />
      </Box>
    </ScrollArea>
  )
}

function DropForm({ admin, p, onCancel, onDone }: { admin: Admin; p: MSPrincipal; onCancel: () => void; onDone: () => void }) {
  const role = isRole(p)
  const [dropLogin, setDropLogin] = useState(p.auth === 'sql' && p.login === p.name)
  const { busy, failure, run } = useRun(admin, onDone)
  const op: AdminOp = role ? { kind: 'ms.role.drop', principal: { name: p.name } }
    : { kind: 'ms.user.drop', principal: { name: p.name, login: dropLogin ? p.login : undefined, dropLogin: dropLogin || undefined } }
  const preview = usePreview(admin, op)
  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box p="md" maw={640}>
        <Text size={FS.body}>
          {role ? `Drops the role ${p.name}. ` : `Drops the user ${p.name} from this database. `}
          {p.owns ? `It owns ${p.owns} schema${p.owns === 1 ? '' : 's'}; SQL Server refuses until they belong to someone else. ` : ''}
          {role && p.members ? `It has ${p.members} member${p.members === 1 ? '' : 's'}; SQL Server refuses until they are taken out. ` : ''}
        </Text>
        {!role && p.login && (p.auth === 'sql' || p.auth === 'windows') && (
          <Checkbox size="xs" mt="md" checked={dropLogin} onChange={(e) => setDropLogin(e.currentTarget.checked)}
            label={`Also drop its login ${p.login} from the server`}
            description="Refused if the login has a user in another database; its open connections end" />
        )}
        <FormFoot sql={preview.sql} previewError={preview.error} failure={failure} busy={busy} canRun verb={`Drop ${role ? 'role' : 'user'}`} danger
          onRun={() => void run(op, 'Dropped', p.name)} onCancel={onCancel} />
      </Box>
    </ScrollArea>
  )
}

export function GrantForm({ admin, grantees, preset, onCancel, onDone }: {
  admin: Admin; grantees: string[]; preset: MSGrantPreset; onCancel: () => void; onDone: (grantee?: string) => void
}) {
  const [kind, setKind] = useState<NonNullable<MSGrantPreset['kind']>>(preset.kind ?? 'ms.grant')
  const [grantee, setGrantee] = useState<string | null>(preset.grantee ?? null)
  const [on, setOn] = useState<On>(preset.on ?? 'schema')
  const [schema, setSchema] = useState<string | null>(preset.schema ?? 'dbo')
  const [objects, setObjects] = useState<string[]>(preset.objects ?? [])
  const [privs, setPrivs] = useState<string[]>(preset.privileges ?? ['SELECT'])
  const [more, setMore] = useState(false)
  const [withGrant, setWithGrant] = useState(false)
  const [cascade, setCascade] = useState(false)
  const cat = useSchemas(admin, on === 'object' ? schema : null)
  const { busy, failure, run } = useRun(admin, () => onDone(grantee ?? undefined))
  const listed = more ? MS_PERMISSIONS[on] : MS_PERMISSIONS[on].filter((p) => COMMON[on].includes(p) || privs.includes(p))
  const op: AdminOp | null = grantee && privs.length && (on === 'database' || schema) && (on !== 'object' || objects.length)
    ? { kind, msGrant: { grantee, on, schema: on === 'database' ? undefined : schema ?? undefined, objects: on === 'object' ? objects : undefined,
      privileges: privs, withGrant: kind !== 'ms.deny' && withGrant ? true : undefined, cascade: kind !== 'ms.grant' && cascade ? true : undefined } }
    : null
  const preview = usePreview(admin, op)
  const verb = kind === 'ms.grant' ? 'Grant' : kind === 'ms.revoke' ? 'Revoke' : 'Deny'
  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box p="md" maw={720}>
        <SegmentedControl size="xs" value={kind} onChange={(v) => setKind(v as typeof kind)}
          data={[{ value: 'ms.grant', label: 'Grant' }, { value: 'ms.revoke', label: 'Revoke' }, { value: 'ms.deny', label: 'Deny' }]} />
        <Text size={FS.micro} c="dimmed" mt={4}>
          {kind === 'ms.grant' ? 'Lets it do these.' : kind === 'ms.revoke' ? 'Takes away a grant or a deny given by name; what it holds through a role stays.'
            : 'Stops it doing these even when a role it is in allows them. A deny always wins.'}
        </Text>
        <Select size="xs" mt="sm" label={kind === 'ms.grant' ? 'Give to' : kind === 'ms.revoke' ? 'Take from' : 'Deny to'} placeholder="Choose a user or role"
          searchable w={320} data={[...grantees.filter((g) => g !== 'public'), 'public']} value={grantee} onChange={setGrantee}
          description={grantee === 'public' ? 'public is every user in this database, now and later' : 'People’s Argus logins get levels from the database’s settings'} />
        <Text size={FS.meta} fw={600} mt="md">On</Text>
        <SegmentedControl size="xs" mt={4} value={on} onChange={(v) => { setOn(v as On); setObjects([]); setPrivs(['SELECT']) }}
          data={[{ value: 'database', label: 'The database' }, { value: 'schema', label: 'A schema' }, { value: 'object', label: 'Tables, views or procedures' }]} />
        {on !== 'database' && (
          <Select size="xs" mt="sm" label="Schema" searchable w={320} data={(cat?.schemas ?? []).map((s) => s.name)} value={schema}
            onChange={(v) => { setSchema(v); setObjects([]) }} description={on === 'schema' ? 'Covers everything in it, now and later' : undefined} />
        )}
        {on === 'object' && schema && (
          <MultiSelect size="xs" mt="sm" label="Which" searchable clearable maxDropdownHeight={260}
            data={(cat?.objects ?? []).map((o) => ({ value: o.name, label: o.kind ? `${o.name} (${o.kind})` : o.name }))}
            value={objects} onChange={setObjects} nothingFoundMessage={cat ? 'None' : 'Loading…'} />
        )}
        <Group mt="md" gap="xs"><Text size={FS.meta} fw={600}>Permissions</Text>
          <Button size="compact-xs" variant="subtle" onClick={() => setMore((m) => !m)}>{more ? 'Fewer' : 'All permissions'}</Button></Group>
        <Group gap="md" mt={4}>
          {listed.map((p) => (
            <Checkbox key={p} size="xs" label={p} checked={privs.includes(p)}
              onChange={(e) => { const c = e.currentTarget.checked; setPrivs((x) => (c ? MS_PERMISSIONS[on].filter((y) => x.includes(y) || y === p) : x.filter((y) => y !== p))) }} />
          ))}
        </Group>
        {kind === 'ms.grant' && on === 'database' && (privs.includes('CONTROL') || privs.includes('TAKE OWNERSHIP')) && (
          <Alert mt="sm" color="amber" variant="light" p="xs"><Text size={FS.meta}>This is the whole database, like db_owner: it waits for someone else’s approval on every database.</Text></Alert>
        )}
        <Box mt="md">
          {kind !== 'ms.deny' && (
            <Checkbox size="xs" checked={withGrant} onChange={(e) => setWithGrant(e.currentTarget.checked)}
              label={kind === 'ms.revoke' ? 'Only take away its right to pass these on (GRANT OPTION FOR)' : 'It may pass these on to others (WITH GRANT OPTION)'} />
          )}
          {kind !== 'ms.grant' && (
            <Checkbox size="xs" mt={6} checked={cascade} onChange={(e) => setCascade(e.currentTarget.checked)}
              label="Also from those it passed them on to (CASCADE)" />
          )}
        </Box>
        <FormFoot sql={preview.sql} previewError={preview.error} failure={failure} busy={busy} canRun={!!op} verb={verb}
          danger={kind !== 'ms.grant'} onRun={() => void run(op, { Grant: 'Granted', Revoke: 'Revoked', Deny: 'Denied' }[verb], grantee ?? '')} onCancel={onCancel} />
        <Tooltip label="Server-wide permissions and logins’ server roles are not changed from a database">
          <Text size={FS.micro} c="dimmed" mt="md">Only this database’s permissions are here.</Text>
        </Tooltip>
      </Box>
    </ScrollArea>
  )
}
