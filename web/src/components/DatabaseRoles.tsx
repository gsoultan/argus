import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Box, Button, Checkbox, Code, Group, Loader, MultiSelect, NumberInput, PasswordInput,
  ScrollArea, SegmentedControl, Select, Switch, Table, Tabs, Text, TextInput, Tooltip,
} from '@mantine/core'
import { IconArrowLeft, IconKey, IconPlus, IconRefresh, IconShieldCheck } from '@tabler/icons-react'
import { FS, INK } from '~/theme'
import { notifyOk } from '~/lib/notify'
import { usePreview } from '~/components/DatabaseExtensions'
import {
  AdminRefused, PanelFrame, ident, literal, type Admin, type AdminFailure, type AdminOp, type AdminPanelProps,
} from '~/components/DatabaseAdmin'
import type { DBLevel } from '~/types/domain'

/*
Roles and privileges, the way pgAdmin manages them: every role on the server,
what one can do in this database, and forms to create, change and drop roles,
change membership, and grant or revoke privileges.

Every change goes to the agent's pg-admin helper as a fixed operation (never
SQL composed here), runs as the server's postgres account inside one
transaction, and is recorded before it runs. Each form shows the agent's own
preview of the statement above its button. Where this database's changes wait
for approval, the session asks for it with that statement and runs it once
approved (DatabaseWorkspace's admin). A password is typed here and turned into
a SCRAM verifier by the agent; the preview, the recording and the approval all
say [redacted].

Giving and taking an Argus level from an application role stays a plain
statement as the DBA level role, as it always was: the level roles are the DBA
level's to hand out WITH ADMIN OPTION.
*/

export interface PGRole {
  name: string
  superuser: boolean
  login: boolean
  inherit: boolean
  createrole: boolean
  createdb: boolean
  replication: boolean
  bypassrls: boolean
  connLimit: number
  validUntil?: string
  hasPassword: boolean
  comment?: string
  memberOf?: string[]
  adminOf?: string[]
  connections: number
  owns: number
  helper?: boolean
}

export interface PGPrivRow {
  schema?: string
  name: string
  kind?: string
  owner: string
  effective: string[] | null
  direct?: string[]
  grantable?: string[]
  objects?: number
}

export interface PGDefaultPriv { forRole: string; schema?: string; objects: string; privileges: string[] }

export interface PGRolePrivileges {
  role: string
  superuser: boolean
  database: PGPrivRow
  schemas: PGPrivRow[]
  tables: PGPrivRow[]
  sequences: PGPrivRow[]
  functions: PGPrivRow[]
  defaults: PGDefaultPriv[]
  truncated?: boolean
}

interface GrantObject { name: string; kind?: string; owner: string }
interface GrantObjects { schemas: GrantObject[]; tables?: GrantObject[]; sequences?: GrantObject[]; functions?: GrantObject[]; owners?: string[]; truncated?: boolean }

export type RoleKind = 'person' | 'level' | 'argus' | 'app'

/** Who a role belongs to, from the mark Argus puts on its own. */
export function roleKind(comment = ''): { kind: RoleKind; label: string; owner?: string } {
  if (comment.startsWith('argus-managed owner=')) return { kind: 'person', label: 'Argus person', owner: comment.slice('argus-managed owner='.length) }
  if (comment.startsWith('argus-managed level')) return { kind: 'level', label: 'Argus level' }
  if (comment.startsWith('argus-managed')) return { kind: 'argus', label: 'Argus' }
  return { kind: 'app', label: 'Application' }
}

const LEVEL_ORDER: DBLevel[] = ['read', 'write', 'owner', 'dba']
const cap = (s: string) => s[0]!.toUpperCase() + s.slice(1)

type On = 'database' | 'schema' | 'table' | 'sequence' | 'function' | 'all-tables' | 'all-sequences' | 'all-functions'
export interface GrantPreset {
  revoke?: boolean
  grantee?: string
  on?: On
  schema?: string
  objects?: string[]
  privileges?: string[]
}

/** The privileges each kind of object takes, as Postgres lists them. */
export const PRIVILEGES: Record<'database' | 'schema' | 'table' | 'sequence' | 'function', string[]> = {
  database: ['CONNECT', 'CREATE', 'TEMPORARY'],
  schema: ['USAGE', 'CREATE'],
  table: ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'TRUNCATE', 'REFERENCES', 'TRIGGER'],
  sequence: ['USAGE', 'SELECT', 'UPDATE'],
  function: ['EXECUTE'],
}
const baseKind = (on: On) => (on.startsWith('all-') ? on.slice(4, -1) : on) as keyof typeof PRIVILEGES

/** What each privilege lets a role do, in words. */
const PRIV_WORDS: Record<string, string> = {
  CONNECT: 'connect to it', CREATE: 'create things in it', TEMPORARY: 'make temporary tables',
  USAGE: 'reach what is inside', SELECT: 'read', INSERT: 'add rows', UPDATE: 'change rows', DELETE: 'delete rows',
  TRUNCATE: 'empty tables', REFERENCES: 'point foreign keys at', TRIGGER: 'add triggers', EXECUTE: 'run',
}

type View =
  | { mode: 'list' }
  | { mode: 'role'; name: string; tab?: string }
  | { mode: 'new' }
  | { mode: 'edit'; role: PGRole }
  | { mode: 'drop'; role: PGRole }
  | { mode: 'grant'; preset: GrantPreset; back: View }

export function RolesManager(props: AdminPanelProps) {
  const { admin, collect, levels, narrow, onBack } = props
  const [roles, setRoles] = useState<PGRole[] | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [busy, setBusy] = useState(false)
  const [filter, setFilter] = useState('')
  const [view, setView] = useState<View>({ mode: 'list' })
  const alive = useRef(true)
  useEffect(() => { alive.current = true; return () => { alive.current = false } }, [])
  const refresh = useCallback(async () => {
    setBusy(true)
    const r = await admin<PGRole[]>({ kind: 'role.list' })
    if (!alive.current) return
    if (r.error) setFailure(r.error)
    else { setFailure(null); setRoles(r.data ?? []) }
    setBusy(false)
  }, [admin])
  useEffect(() => { void refresh() }, [refresh])

  const levelOf = useMemo(() => {
    const byRole = new Map<string, string>()
    for (const l of LEVEL_ORDER) if (levels[l]) byRole.set(levels[l]!, l)
    return byRole
  }, [levels])
  const back = () => setView({ mode: 'list' })
  const done = (next: View = { mode: 'list' }) => { setView(next); void refresh() }

  // Giving and taking an Argus level stays a recorded statement as the DBA
  // level role, which holds the level roles WITH ADMIN OPTION.
  const runLevel = async (sql: string, confirmText?: string) => {
    if (confirmText && !window.confirm(confirmText)) return
    const r = await collect(sql, 1)
    if (r.error) window.alert(`Postgres refused this: ${r.error.error ?? ''}`)
    void refresh()
  }

  const names = (roles ?? []).map((r) => r.name)
  const header = (
    <Group gap={6} wrap="nowrap">
      <Button size="compact-sm" variant="default" leftSection={<IconKey size={14} />}
        onClick={() => setView({ mode: 'grant', preset: {}, back: { mode: 'list' } })}>Grant…</Button>
      <Button size="compact-sm" leftSection={<IconPlus size={14} />} onClick={() => setView({ mode: 'new' })}>New role</Button>
      <ActionIcon variant="subtle" onClick={() => void refresh()} loading={busy} aria-label="Refresh"><IconRefresh size={16} /></ActionIcon>
    </Group>
  )

  if (view.mode === 'new' || view.mode === 'edit') {
    return (
      <PanelFrame title={view.mode === 'new' ? 'New role' : `Change ${view.role.name}`} narrow={narrow} onBack={onBack}
        blurb="Roles are server-wide: a role made here can sign in to every database on this server that lets it.">
        <RoleForm admin={admin} role={view.mode === 'edit' ? view.role : undefined} roles={roles ?? []}
          onCancel={() => setView(view.mode === 'edit' ? { mode: 'role', name: view.role.name, tab: 'settings' } : { mode: 'list' })}
          onDone={(name) => done({ mode: 'role', name, tab: view.mode === 'new' ? 'privileges' : 'settings' })} />
      </PanelFrame>
    )
  }
  if (view.mode === 'drop') {
    return (
      <PanelFrame title={`Drop ${view.role.name}`} narrow={narrow} onBack={onBack} blurb="Removing a role from the server.">
        <DropForm admin={admin} role={view.role} roles={names} onCancel={() => setView({ mode: 'role', name: view.role.name })} onDone={() => done()} />
      </PanelFrame>
    )
  }
  if (view.mode === 'grant') {
    return (
      <PanelFrame title={view.preset.revoke ? 'Revoke privileges' : 'Grant privileges'} narrow={narrow} onBack={onBack}
        blurb="What a role may do with this database and what is in it. Runs as the server’s postgres account, so it takes effect whoever owns the objects.">
        <GrantForm admin={admin} roles={(roles ?? []).filter((r) => roleKind(r.comment).kind === 'app')} preset={view.preset}
          onCancel={() => setView(view.back)}
          onDone={(grantee) => done(grantee && grantee !== 'PUBLIC' ? { mode: 'role', name: grantee, tab: 'privileges' } : view.back)} />
      </PanelFrame>
    )
  }
  if (view.mode === 'role') {
    const role = roles?.find((r) => r.name === view.name)
    return (
      <PanelFrame title={view.name} narrow={narrow} onBack={onBack} blurb="What this role is, what it belongs to, and what it can do in this database." right={header}>
        <RoleDetail key={view.name} admin={admin} name={view.name} role={role} roles={roles ?? []} tab={view.tab} narrow={narrow}
          onBack={back} onChanged={() => void refresh()}
          onEdit={(r) => setView({ mode: 'edit', role: r })} onDrop={(r) => setView({ mode: 'drop', role: r })}
          onGrant={(preset) => setView({ mode: 'grant', preset, back: { mode: 'role', name: view.name, tab: 'privileges' } })} />
      </PanelFrame>
    )
  }

  const q = filter.trim().toLowerCase()
  const shown = (roles ?? []).filter((r) => !q || r.name.toLowerCase().includes(q) || (r.comment ?? '').toLowerCase().includes(q))
  return (
    <PanelFrame title="Roles & access" narrow={narrow} onBack={onBack} right={header}
      blurb="Every role on this server: create and change them, grant what they may do here, and see what each one can actually do.">
      <Group gap="sm" px="md" py={6} style={{ borderBottom: '1px solid var(--color-line)' }} wrap="wrap">
        <TextInput size="xs" placeholder={`Filter ${roles?.length ?? ''} roles…`} value={filter} onChange={(e) => setFilter(e.currentTarget.value)}
          w={narrow ? '100%' : 240} aria-label="Filter roles" />
        {!narrow && <Text size={FS.micro} c="dimmed">Levels here:</Text>}
        {!narrow && LEVEL_ORDER.map((l) => levels[l] && <Badge key={l} size="xs" variant="light" color="slate" radius="sm" ff="monospace" tt="none">{levels[l]}</Badge>)}
      </Group>
      {failure ? <Box p="md"><AdminRefused error={failure} /></Box> : !roles ? <Loader size={16} m="md" /> : (
        <ScrollArea style={{ flex: 1 }} type="auto">
          <Table verticalSpacing={4} fz={12} striped highlightOnHover>
            <Table.Thead style={{ position: 'sticky', top: 0, background: 'var(--color-raised)', zIndex: 1 }}>
              <Table.Tr>
                <Table.Th>Role</Table.Th><Table.Th>Kind</Table.Th><Table.Th>Can log in</Table.Th>
                {!narrow && <Table.Th>Member of</Table.Th>}
                <Table.Th>Level here</Table.Th>{!narrow && <Table.Th>Connections</Table.Th>}<Table.Th></Table.Th>
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {shown.map((r) => {
                const k = roleKind(r.comment)
                const held = LEVEL_ORDER.filter((l) => levels[l] && (r.memberOf ?? []).includes(levels[l]!))
                const others = (r.memberOf ?? []).filter((g) => !levelOf.has(g))
                return (
                  <Table.Tr key={r.name}>
                    <Table.Td>
                      <Text size={FS.meta} ff="monospace" fw={600} c={INK.brand} style={{ cursor: 'pointer' }} role="button" tabIndex={0}
                        onClick={() => setView({ mode: 'role', name: r.name })}
                        onKeyDown={(e) => { if (e.key === 'Enter') setView({ mode: 'role', name: r.name }) }}>{r.name}</Text>
                      {k.owner && <Text size={FS.micro} c="dimmed">{k.owner}</Text>}
                      {k.kind === 'app' && r.comment && !narrow && <Text size={FS.micro} c="dimmed" lineClamp={1}>{r.comment}</Text>}
                    </Table.Td>
                    <Table.Td>
                      <Group gap={4}>
                        <Badge size="xs" variant="light" color={r.superuser ? 'amber' : k.kind === 'app' ? 'teal' : k.kind === 'person' ? 'azure' : 'slate'} radius="sm">
                          {r.superuser ? 'Superuser' : k.label}
                        </Badge>
                        {r.createrole && !r.superuser && <Badge size="xs" variant="outline" color="slate" radius="sm">createrole</Badge>}
                        {r.helper && <Tooltip label="The account Argus manages this server with"><Badge size="xs" variant="outline" color="slate" radius="sm">Argus helper</Badge></Tooltip>}
                      </Group>
                    </Table.Td>
                    <Table.Td>{r.login ? 'yes' : <span style={{ color: 'var(--mantine-color-dimmed)' }}>no</span>}</Table.Td>
                    {!narrow && (
                      <Table.Td>
                        <Group gap={4}>{others.slice(0, 4).map((g) => <Badge key={g} size="xs" variant="outline" color="slate" radius="sm" tt="none" ff="monospace">{g}</Badge>)}
                          {others.length > 4 && <Text size={FS.micro} c="dimmed">+{others.length - 4}</Text>}</Group>
                      </Table.Td>
                    )}
                    <Table.Td>
                      <Group gap={4}>
                        {held.map((l) => <Badge key={l} size="xs" variant="light" color={l === 'read' ? 'slate' : 'azure'} radius="sm">{l}</Badge>)}
                        {k.kind === 'level' && <Text size={FS.micro} c="dimmed">is {levelOf.get(r.name) ? `the ${levelOf.get(r.name)} level` : 'a level'}</Text>}
                      </Group>
                    </Table.Td>
                    {!narrow && <Table.Td>{r.connections || <span style={{ color: 'var(--mantine-color-dimmed)' }}>0</span>}</Table.Td>}
                    <Table.Td ta="right">
                      <RowActions role={r} app={k.kind === 'app' && !r.superuser} levels={levels} held={held} run={runLevel}
                        onOpen={() => setView({ mode: 'role', name: r.name })}
                        onGrant={() => setView({ mode: 'grant', preset: { grantee: r.name }, back: { mode: 'list' } })} />
                    </Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
          {shown.length === 0 && <Text size={FS.meta} c="dimmed" p="md">No role matches.</Text>}
          <Text size={FS.micro} c="dimmed" p="md">
            People who open this database through Argus are given levels from its settings, not here; their roles are locked between sessions.
            Every change on this page runs as the server’s postgres account and is in this session’s recording.
          </Text>
        </ScrollArea>
      )}
    </PanelFrame>
  )
}

/** The quick actions on one row, as a plain select (light, and the same on a phone). */
function RowActions({ role, app, levels, held, run, onOpen, onGrant }: {
  role: PGRole; app: boolean; levels: Partial<Record<DBLevel, string>>; held: DBLevel[]
  run: (sql: string, confirmText?: string) => Promise<void>; onOpen: () => void; onGrant: () => void
}) {
  const onChange = (e: React.ChangeEvent<HTMLSelectElement>) => {
    const v = e.currentTarget.value
    e.currentTarget.value = ''
    if (v === 'open') return onOpen()
    if (v === 'grant') return onGrant()
    const [kind, l] = v.split(':') as [string, DBLevel]
    if (kind === 'give' && levels[l]) void run(`GRANT ${ident(levels[l]!)} TO ${ident(role.name)}`)
    if (kind === 'take' && levels[l]) {
      void run(`REVOKE ${ident(levels[l]!)} FROM ${ident(role.name)}`,
        `Take ${l} on this database from ${role.name}? Its own grants are untouched; only the Argus level goes.`)
    }
    if (kind === 'end') {
      void run(`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity WHERE usename = ${literal(role.name)} AND datname = current_database() AND pid <> pg_backend_pid()`,
        `End all ${role.connections} connection(s) of ${role.name} to this database? Their transactions are rolled back.`)
    }
  }
  return (
    <select defaultValue="" onChange={onChange} aria-label={`Actions for ${role.name}`}
      style={{ fontSize: 12, padding: '2px 6px', borderRadius: 6, border: '1px solid var(--color-line)', background: 'var(--color-surface)', color: 'inherit', maxWidth: 170 }}>
      <option value="" disabled>Actions…</option>
      <option value="open">What it can do here</option>
      {app && <option value="grant">Grant privileges…</option>}
      {app && (['read', 'write', 'owner'] as DBLevel[]).filter((l) => levels[l] && !held.includes(l)).map((l) => (
        <option key={'g' + l} value={`give:${l}`}>Give Argus {cap(l)} level</option>
      ))}
      {app && held.map((l) => <option key={'t' + l} value={`take:${l}`}>Take back {cap(l)} level</option>)}
      {role.connections > 0 && !role.superuser && <option value="end">End its {role.connections} connection{role.connections === 1 ? '' : 's'}</option>}
    </select>
  )
}

/* ── One role ───────────────────────────────────────────────────────────── */

function RoleDetail({ admin, name, role, roles, tab, narrow, onBack, onChanged, onEdit, onDrop, onGrant }: {
  admin: Admin; name: string; role?: PGRole; roles: PGRole[]; tab?: string; narrow: boolean
  onBack: () => void; onChanged: () => void; onEdit: (r: PGRole) => void; onDrop: (r: PGRole) => void; onGrant: (p: GrantPreset) => void
}) {
  const [privs, setPrivs] = useState<PGRolePrivileges | null>(null)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [active, setActive] = useState<string>(tab ?? 'privileges')
  const load = useCallback(async () => {
    const r = await admin<PGRolePrivileges>({ kind: 'role.privileges', role: { name } })
    if (r.error) setFailure(r.error)
    else { setFailure(null); setPrivs(r.data ?? null) }
  }, [admin, name])
  useEffect(() => { void load() }, [load])
  const k = roleKind(role?.comment)
  const managed = k.kind !== 'app'

  const revoke = async (preset: GrantPreset & { future?: boolean; forRoles?: string[] }, words: string) => {
    if (!window.confirm(`${words}?`)) return
    const r = await admin({ kind: 'revoke', grant: { grantee: name, on: preset.on ?? 'table', schema: preset.schema, objects: preset.objects,
      privileges: preset.privileges ?? [], future: preset.future, forRoles: preset.forRoles } })
    if (r.error) { setFailure(r.error); return }
    notifyOk('Revoked', words)
    void load()
  }

  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box px="md" pt="sm">
        <Button size="compact-xs" variant="subtle" leftSection={<IconArrowLeft size={12} />} onClick={onBack}>All roles</Button>
        {role && (
          <Group gap={6} mt={6} wrap="wrap">
            <Badge size="sm" variant="light" color={role.superuser ? 'amber' : k.kind === 'app' ? 'teal' : 'slate'} radius="sm">{role.superuser ? 'Superuser' : k.label}</Badge>
            <Badge size="sm" variant="outline" color="slate" radius="sm">{role.login ? 'can log in' : 'cannot log in'}</Badge>
            {role.login && !role.hasPassword && <Badge size="sm" variant="light" color="amber" radius="sm">no password</Badge>}
            {role.validUntil && <Badge size="sm" variant="outline" color="slate" radius="sm">password until {role.validUntil}</Badge>}
            {role.connLimit >= 0 && <Badge size="sm" variant="outline" color="slate" radius="sm">at most {role.connLimit} connections</Badge>}
            {k.owner && <Text size={FS.meta} c="dimmed">for {k.owner}</Text>}
          </Group>
        )}
        {role?.comment && k.kind === 'app' && <Text size={FS.meta} c="dimmed" mt={4}>{role.comment}</Text>}
        {managed && role && (
          <Alert mt="sm" color="slate" variant="light" p="xs">
            <Text size={FS.meta}>{k.kind === 'person'
              ? 'This is a person’s role that Argus manages. Give them a level from the database’s settings; it is locked between their sessions.'
              : 'This is one of Argus’s own roles. It is managed from the database’s settings, so it cannot be changed here.'}</Text>
          </Alert>
        )}
        {!managed && role && (
          <Group gap="xs" mt="sm">
            <Button size="xs" leftSection={<IconKey size={14} />} onClick={() => onGrant({ grantee: name })}>Grant…</Button>
            <Button size="xs" variant="default" onClick={() => onEdit(role)} disabled={!role}>Change role</Button>
            {!role.helper && <Button size="xs" variant="subtle" color="rose" onClick={() => onDrop(role)}>Drop role</Button>}
          </Group>
        )}
      </Box>
      <Tabs value={active} onChange={(v) => setActive(v ?? 'privileges')} mt="sm" keepMounted={false}>
        <Tabs.List px="md">
          <Tabs.Tab value="privileges">What it can do here</Tabs.Tab>
          <Tabs.Tab value="members">Member of{role?.memberOf?.length ? ` (${role.memberOf.length})` : ''}</Tabs.Tab>
        </Tabs.List>
        <Tabs.Panel value="privileges">
          {failure && <Box p="md"><AdminRefused error={failure} /></Box>}
          {!privs && !failure ? <Loader size={16} m="md" /> : privs && (
            <PrivilegesView privs={privs} canChange={!managed} narrow={narrow} onRevoke={revoke}
              onGrant={(p) => onGrant({ grantee: name, ...p })} />
          )}
        </Tabs.Panel>
        <Tabs.Panel value="members">
          {role && <Membership admin={admin} role={role} roles={roles} canChange={!managed} onChanged={onChanged} />}
        </Tabs.Panel>
      </Tabs>
    </ScrollArea>
  )
}

function PrivBadges({ row }: { row: PGPrivRow }) {
  const eff = row.effective ?? []
  const direct = new Set(row.direct ?? [])
  const grantable = new Set(row.grantable ?? [])
  if (eff.length === 0) return <Text size={FS.micro} c="dimmed">nothing</Text>
  return (
    <Group gap={4} wrap="wrap">
      {eff.map((p) => (
        <Tooltip key={p} label={`${PRIV_WORDS[p] ?? p}${direct.has(p) ? ', granted to this role' : ', through a role it is in, PUBLIC or owning it'}${grantable.has(p) ? '; may pass it on' : ''}`}>
          <Badge size="xs" radius="sm" tt="none" variant={direct.has(p) ? 'light' : 'outline'} color={direct.has(p) ? 'azure' : 'slate'}>
            {p}{grantable.has(p) ? '*' : ''}
          </Badge>
        </Tooltip>
      ))}
    </Group>
  )
}

function Section({ title, children, right }: { title: string; children: React.ReactNode; right?: React.ReactNode }) {
  return (
    <>
      <Group px="md" pt="md" pb={4} gap="sm" wrap="nowrap">
        <Text size={FS.micro} c="dimmed" tt="uppercase" fw={600} style={{ letterSpacing: '.06em', flex: 1 }}>{title}</Text>
        {right}
      </Group>
      {children}
    </>
  )
}

function PrivilegesView({ privs, canChange, narrow, onRevoke, onGrant }: {
  privs: PGRolePrivileges; canChange: boolean; narrow: boolean
  onRevoke: (p: GrantPreset & { future?: boolean; forRoles?: string[] }, words: string) => void; onGrant: (p: GrantPreset) => void
}) {
  const bySchema = useMemo(() => {
    const m = new Map<string, PGPrivRow[]>()
    for (const t of privs.tables) m.set(t.schema ?? '', [...(m.get(t.schema ?? '') ?? []), t])
    return m
  }, [privs.tables])
  const usable = privs.schemas.filter((s) => (s.effective ?? []).length > 0 || bySchema.has(s.name))
  const revokeBtn = (p: GrantPreset & { future?: boolean; forRoles?: string[] }, words: string) => canChange && (
    <Button size="compact-xs" variant="subtle" color="rose" onClick={() => onRevoke(p, words)}>Revoke</Button>
  )
  const directOnly = (r: PGPrivRow) => (r.direct ?? []).length > 0

  if (privs.superuser) {
    return (
      <Alert m="md" color="amber" variant="light" icon={<IconShieldCheck size={16} />}>
        <Text size={FS.meta}>{privs.role} is a superuser: it can do everything on this server, whatever is granted. Grants only start to matter once that is taken away.</Text>
      </Alert>
    )
  }
  return (
    <Box pb="md">
      <Text size={FS.micro} c="dimmed" px="md" pt="sm">
        Filled badges were granted to {privs.role} itself and can be revoked here; outlined ones come through a role it is in, through PUBLIC, or from owning the object. * means it may pass the privilege on.
      </Text>
      <Section title="This database" right={canChange && <Button size="compact-xs" variant="default" onClick={() => onGrant({ on: 'database' })}>Grant…</Button>}>
        <Table verticalSpacing={4} fz={12}>
          <Table.Tbody>
            <Table.Tr>
              <Table.Td><Text size={FS.meta} ff="monospace">{privs.database.name}</Text></Table.Td>
              <Table.Td><PrivBadges row={privs.database} /></Table.Td>
              <Table.Td ta="right">{directOnly(privs.database) && revokeBtn({ on: 'database', privileges: privs.database.direct }, `Revoke ${privs.database.direct!.join(', ')} on database ${privs.database.name} from ${privs.role}`)}</Table.Td>
            </Table.Tr>
          </Table.Tbody>
        </Table>
        {!(privs.database.effective ?? []).includes('CONNECT') && (
          <Text size={FS.meta} c={INK.pending} px="md">{privs.role} cannot connect to this database.</Text>
        )}
      </Section>

      <Section title="Schemas">
        <Table verticalSpacing={4} fz={12} striped>
          <Table.Thead><Table.Tr><Table.Th>Schema</Table.Th>{!narrow && <Table.Th>Owner</Table.Th>}<Table.Th>On the schema</Table.Th><Table.Th>Tables it can use</Table.Th><Table.Th></Table.Th></Table.Tr></Table.Thead>
          <Table.Tbody>
            {privs.schemas.map((s) => {
              const usage = (s.effective ?? []).includes('USAGE')
              const tables = bySchema.get(s.name)?.length ?? 0
              return (
                <Table.Tr key={s.name} style={!usable.includes(s) ? { opacity: 0.6 } : undefined}>
                  <Table.Td><Text size={FS.meta} ff="monospace">{s.name}</Text></Table.Td>
                  {!narrow && <Table.Td><Text size={FS.micro} ff="monospace" c="dimmed">{s.owner}</Text></Table.Td>}
                  <Table.Td><PrivBadges row={s} /></Table.Td>
                  <Table.Td>
                    <Text size={FS.meta}>{tables} of {s.objects ?? 0}</Text>
                    {tables > 0 && !usage && <Text size={FS.micro} c={INK.pending}>no USAGE on the schema, so none of them can be reached</Text>}
                  </Table.Td>
                  <Table.Td ta="right">
                    <Group gap={4} justify="flex-end" wrap="nowrap">
                      {canChange && <Button size="compact-xs" variant="default" onClick={() => onGrant({ on: 'all-tables', schema: s.name })}>Grant…</Button>}
                      {directOnly(s) && revokeBtn({ on: 'schema', schema: s.name, privileges: s.direct }, `Revoke ${s.direct!.join(', ')} on schema ${s.name} from ${privs.role}`)}
                    </Group>
                  </Table.Td>
                </Table.Tr>
              )
            })}
          </Table.Tbody>
        </Table>
      </Section>

      <Section title={`Tables and views (${privs.tables.length}${privs.truncated ? '+' : ''})`}>
        {privs.tables.length === 0 ? <Text size={FS.meta} c="dimmed" px="md">It can use no table or view in this database.</Text> : (
          <Table verticalSpacing={4} fz={12} striped highlightOnHover>
            <Table.Thead><Table.Tr><Table.Th>Table</Table.Th>{!narrow && <Table.Th>Owner</Table.Th>}<Table.Th>It can</Table.Th><Table.Th></Table.Th></Table.Tr></Table.Thead>
            <Table.Tbody>
              {privs.tables.map((t) => (
                <Table.Tr key={`${t.schema}.${t.name}`}>
                  <Table.Td>
                    <Text size={FS.meta} ff="monospace">{t.schema}.{t.name}</Text>
                    {t.kind && t.kind !== 'table' && <Text size={FS.micro} c="dimmed">{t.kind}</Text>}
                  </Table.Td>
                  {!narrow && <Table.Td><Text size={FS.micro} ff="monospace" c="dimmed">{t.owner}</Text></Table.Td>}
                  <Table.Td><PrivBadges row={t} /></Table.Td>
                  <Table.Td ta="right">{directOnly(t) && revokeBtn({ on: 'table', schema: t.schema, objects: [t.name], privileges: t.direct }, `Revoke ${t.direct!.join(', ')} on ${t.schema}.${t.name} from ${privs.role}`)}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        )}
      </Section>

      {privs.sequences.length > 0 && (
        <Section title={`Sequences (${privs.sequences.length})`}>
          <Table verticalSpacing={4} fz={12} striped>
            <Table.Tbody>
              {privs.sequences.map((t) => (
                <Table.Tr key={`${t.schema}.${t.name}`}>
                  <Table.Td><Text size={FS.meta} ff="monospace">{t.schema}.{t.name}</Text></Table.Td>
                  <Table.Td><PrivBadges row={t} /></Table.Td>
                  <Table.Td ta="right">{directOnly(t) && revokeBtn({ on: 'sequence', schema: t.schema, objects: [t.name], privileges: t.direct }, `Revoke ${t.direct!.join(', ')} on sequence ${t.schema}.${t.name} from ${privs.role}`)}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Section>
      )}

      {privs.functions.length > 0 && (
        <Section title={`Functions granted to it or that it owns (${privs.functions.length})`}>
          <Table verticalSpacing={4} fz={12} striped>
            <Table.Tbody>
              {privs.functions.map((t) => (
                <Table.Tr key={`${t.schema}.${t.name}`}>
                  <Table.Td><Text size={FS.meta} ff="monospace">{t.schema}.{t.name}</Text></Table.Td>
                  <Table.Td>{(t.direct ?? []).length ? <PrivBadges row={t} /> : <Text size={FS.micro} c="dimmed">owns it</Text>}</Table.Td>
                  <Table.Td ta="right">{directOnly(t) && revokeBtn({ on: 'function', schema: t.schema, objects: [t.name], privileges: ['EXECUTE'] }, `Revoke EXECUTE on ${t.schema}.${t.name} from ${privs.role}`)}</Table.Td>
                </Table.Tr>
              ))}
            </Table.Tbody>
          </Table>
        </Section>
      )}

      <Section title="On what is made later (default privileges)">
        {privs.defaults.length === 0 ? (
          <Text size={FS.meta} c="dimmed" px="md">Nothing. New tables in a schema reach {privs.role} only when granted again, or when a grant here covered “ones made later”.</Text>
        ) : (
          <Table verticalSpacing={4} fz={12} striped>
            <Table.Thead><Table.Tr><Table.Th>When</Table.Th><Table.Th>It gets</Table.Th><Table.Th></Table.Th></Table.Tr></Table.Thead>
            <Table.Tbody>
              {privs.defaults.map((d) => {
                const on = ({ tables: 'all-tables', sequences: 'all-sequences', functions: 'all-functions' } as Record<string, On>)[d.objects]
                return (
                  <Table.Tr key={`${d.forRole}/${d.schema}/${d.objects}`}>
                    <Table.Td><Text size={FS.meta}><Text span ff="monospace">{d.forRole}</Text> makes {d.objects}{d.schema ? <> in <Text span ff="monospace">{d.schema}</Text></> : ' anywhere'}</Text></Table.Td>
                    <Table.Td><Group gap={4}>{d.privileges.map((p) => <Badge key={p} size="xs" radius="sm" tt="none" variant="light" color="azure">{p}</Badge>)}</Group></Table.Td>
                    <Table.Td ta="right">{on && d.schema && canChange && (
                      <Button size="compact-xs" variant="subtle" color="rose"
                        onClick={() => onRevoke({ on, schema: d.schema, privileges: d.privileges, future: true, forRoles: [d.forRole] },
                          `Stop giving ${privs.role} ${d.privileges.join(', ')} on ${d.objects} ${d.forRole} makes in ${d.schema} (this also revokes them on the ones already there)`)}>Stop</Button>
                    )}</Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        )}
      </Section>
    </Box>
  )
}

function Membership({ admin, role, roles, canChange, onChanged }: {
  admin: Admin; role: PGRole; roles: PGRole[]; canChange: boolean; onChanged: () => void
}) {
  const [pick, setPick] = useState<string | null>(null)
  const [withAdmin, setWithAdmin] = useState(false)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const [busy, setBusy] = useState(false)
  const member = role.memberOf ?? []
  const choices = roles.filter((r) => r.name !== role.name && !member.includes(r.name) && ['app', 'level'].includes(roleKind(r.comment).kind) && !r.superuser)
    .map((r) => ({ value: r.name, label: roleKind(r.comment).kind === 'level' ? `${r.name} (Argus level)` : r.name }))
  const op: AdminOp | null = pick ? { kind: 'role.member', member: { role: pick, member: role.name, admin: withAdmin } } : null
  const preview = usePreview(admin, canChange ? op : null)
  const change = async (o: AdminOp, words: string) => {
    setBusy(true)
    const r = await admin(o)
    setBusy(false)
    if (r.error) { setFailure(r.error); return }
    setFailure(null)
    setPick(null)
    notifyOk('Membership changed', words)
    onChanged()
  }
  return (
    <Box p="md" maw={720}>
      <Text size={FS.meta} c="dimmed">A role holds what every role it is a member of holds{role.inherit ? '' : ' — but this one does not inherit, so it must SET ROLE to use them'}.</Text>
      <Table verticalSpacing={4} fz={12} mt="xs" striped>
        <Table.Tbody>
          {member.length === 0 && <Table.Tr><Table.Td><Text size={FS.meta} c="dimmed">It is in no other role.</Text></Table.Td></Table.Tr>}
          {member.map((g) => (
            <Table.Tr key={g}>
              <Table.Td><Text size={FS.meta} ff="monospace">{g}</Text></Table.Td>
              <Table.Td>{(role.adminOf ?? []).includes(g) && <Badge size="xs" variant="outline" color="slate" radius="sm">may give it to others</Badge>}</Table.Td>
              <Table.Td ta="right">{canChange && (
                <Button size="compact-xs" variant="subtle" color="rose" loading={busy}
                  onClick={() => { if (window.confirm(`Take ${role.name} out of ${g}?`)) void change({ kind: 'role.unmember', member: { role: g, member: role.name } }, `${role.name} left ${g}`) }}>Remove</Button>
              )}</Table.Td>
            </Table.Tr>
          ))}
        </Table.Tbody>
      </Table>
      {canChange && (
        <>
          <Group mt="md" align="flex-end" gap="sm">
            <Select size="xs" label="Add to a role" placeholder="Choose a role" data={choices} value={pick} onChange={setPick} searchable w={260} />
            <Checkbox size="xs" mb={6} label="May give it to others (WITH ADMIN OPTION)" checked={withAdmin} onChange={(e) => setWithAdmin(e.currentTarget.checked)} />
          </Group>
          {op && (
            <>
              <Code block mt="sm" fz={12}>{preview.sql || (preview.error ? `— ${preview.error.error}` : '…')}</Code>
              <Button size="xs" mt="sm" loading={busy} disabled={!preview.sql} onClick={() => void change(op, `${role.name} joined ${pick}`)}>Add</Button>
            </>
          )}
        </>
      )}
      {failure && <Box mt="sm"><AdminRefused error={failure} /></Box>}
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
      <Text size={FS.micro} c="dimmed" mt="md">This runs as the server’s postgres account, in one transaction, and is recorded in this session. On a database whose changes wait for approval, it is asked for first.</Text>
      <Code block mt={4} fz={12}>{sql || (previewError ? `— ${previewError.error}` : '…')}</Code>
      {failure && <Box mt="sm"><AdminRefused error={failure} /></Box>}
      <Group mt="md" gap="xs">
        <Button size="xs" color={danger ? 'rose' : undefined} onClick={onRun} disabled={!canRun || !sql} loading={busy}>{verb}</Button>
        <Button size="xs" variant="default" onClick={onCancel}>Cancel</Button>
      </Group>
    </>
  )
}

const FLAGS: { key: 'createdb' | 'createrole' | 'replication' | 'bypassrls' | 'inherit'; label: string; hint: string }[] = [
  { key: 'inherit', label: 'Inherits what its roles hold', hint: 'Off means it must SET ROLE to use a role it is in' },
  { key: 'createdb', label: 'Can create databases', hint: 'CREATEDB' },
  { key: 'createrole', label: 'Can create and change roles', hint: 'CREATEROLE: close to an administrator' },
  { key: 'replication', label: 'Can stream replication', hint: 'REPLICATION: for replicas and backup tools' },
  { key: 'bypassrls', label: 'Ignores row-level security', hint: 'BYPASSRLS' },
]

function RoleForm({ admin, role, roles, onCancel, onDone }: {
  admin: Admin; role?: PGRole; roles: PGRole[]; onCancel: () => void; onDone: (name: string) => void
}) {
  const editing = !!role
  const [name, setName] = useState(role?.name ?? '')
  const [login, setLogin] = useState(role?.login ?? true)
  const [password, setPassword] = useState('')
  const [noPassword, setNoPassword] = useState(false)
  const [until, setUntil] = useState(role?.validUntil?.replace(/ UTC$/, '') ?? '')
  const [limit, setLimit] = useState<number | ''>(role ? (role.connLimit < 0 ? '' : role.connLimit) : '')
  const [comment, setComment] = useState(role?.comment ?? '')
  const [flags, setFlags] = useState({ inherit: role?.inherit ?? true, createdb: role?.createdb ?? false, createrole: role?.createrole ?? false,
    replication: role?.replication ?? false, bypassrls: role?.bypassrls ?? false })
  const [superuser, setSuperuser] = useState(role?.superuser ?? false)
  const [memberOf, setMemberOf] = useState<string[]>([])
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<AdminFailure | null>(null)

  // On change, only what differs is sent, so the statement says exactly what changes.
  const spec: Record<string, unknown> = { name: editing ? role!.name : name.trim() }
  const diff = <T,>(k: string, now: T, was: T | undefined) => { if (!editing || now !== was) spec[k] = now }
  diff('login', login, role?.login)
  for (const f of FLAGS) diff(f.key, flags[f.key], role?.[f.key])
  diff('superuser', superuser, role?.superuser)
  const limitNow = limit === '' ? -1 : limit
  if (!editing ? limitNow !== -1 : limitNow !== role!.connLimit) spec.connLimit = limitNow
  const untilNow = until.trim()
  if (!editing ? untilNow !== '' : untilNow !== (role!.validUntil?.replace(/ UTC$/, '') ?? '')) spec.validUntil = untilNow === '' ? 'infinity' : untilNow
  if (password) spec.password = password
  if (noPassword) spec.noPassword = true
  if (!editing ? comment.trim() !== '' : comment.trim() !== (role!.comment ?? '')) spec.comment = comment.trim()
  if (!editing && memberOf.length) spec.memberOf = memberOf
  if (editing && name.trim() && name.trim() !== role!.name) spec.rename = name.trim()
  const op: AdminOp = { kind: editing ? 'role.alter' : 'role.create', role: spec as AdminOp['role'] }
  const ready = !!(editing || name.trim())
  const preview = usePreview(admin, ready ? op : null)
  const groups = roles.filter((r) => !r.login && !r.superuser && ['app', 'level'].includes(roleKind(r.comment).kind) && r.name !== role?.name)
    .map((r) => ({ value: r.name, label: roleKind(r.comment).kind === 'level' ? `${r.name} (Argus level)` : r.name }))

  const run = async () => {
    setBusy(true)
    const r = await admin({ ...op })
    setBusy(false)
    if (r.error) { setFailure(r.error); return }
    setPassword('')
    const final = (spec.rename as string | undefined) ?? (spec.name as string)
    notifyOk(editing ? `${final} changed` : `${final} created`, r.tag ?? 'Done')
    onDone(final)
  }

  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box p="md" maw={680}>
        <Group grow align="flex-start">
          <TextInput size="xs" label={editing ? 'Name (change to rename)' : 'Name'} value={name} onChange={(e) => setName(e.currentTarget.value)}
            placeholder="hermod" autoComplete="off" disabled={editing && role!.helper} data-autofocus />
          <Switch size="sm" mt={22} label="Can log in" checked={login} onChange={(e) => setLogin(e.currentTarget.checked)} disabled={editing && role!.helper} />
        </Group>
        {login && (
          <Group grow align="flex-start" mt="sm">
            <PasswordInput size="xs" label={editing ? (role!.hasPassword ? 'New password (leave empty to keep it)' : 'Password (it has none)') : 'Password'}
              value={password} onChange={(e) => { setPassword(e.currentTarget.value); if (e.currentTarget.value) setNoPassword(false) }} autoComplete="new-password"
              description="Sent to the server only as a SCRAM verifier; never recorded" />
            <TextInput size="xs" label="Password works until" placeholder="never (e.g. 2027-01-31)" value={until} onChange={(e) => setUntil(e.currentTarget.value)}
              description="A date, or a date and time with UTC" />
          </Group>
        )}
        {editing && role!.hasPassword && (
          <Checkbox size="xs" mt="xs" label="Remove its password (it can then sign in only where pg_hba.conf trusts it)" checked={noPassword}
            onChange={(e) => { setNoPassword(e.currentTarget.checked); if (e.currentTarget.checked) setPassword('') }} />
        )}
        <Group grow align="flex-start" mt="sm">
          <NumberInput size="xs" label="Connections at once" placeholder="no limit" min={0} max={100000} value={limit} onChange={(v) => setLimit(v === '' ? '' : Number(v))} />
          <TextInput size="xs" label="Description" value={comment} onChange={(e) => setComment(e.currentTarget.value)} placeholder="What uses this role" />
        </Group>
        {!editing && (
          <MultiSelect size="xs" mt="sm" label="Member of" description="It holds what these roles hold" data={groups} value={memberOf} onChange={setMemberOf} searchable clearable />
        )}
        <Text size={FS.meta} fw={600} mt="md">What it may do on the server</Text>
        {FLAGS.map((f) => (
          <Switch key={f.key} size="xs" mt={6} label={f.label} description={f.hint} checked={flags[f.key]}
            onChange={(e) => { const v = e.currentTarget.checked; setFlags((x) => ({ ...x, [f.key]: v })) }} />
        ))}
        <Switch size="xs" mt={6} color="amber" label="Superuser" checked={superuser} disabled={editing && role!.helper}
          description="Everything, on every database here, including reading the server’s files. Always waits for someone else to approve it."
          onChange={(e) => setSuperuser(e.currentTarget.checked)} />
        <FormFoot sql={preview.sql} previewError={preview.error} failure={failure} busy={busy} canRun={ready}
          verb={editing ? 'Save changes' : 'Create role'} onRun={() => void run()} onCancel={onCancel} />
      </Box>
    </ScrollArea>
  )
}

function DropForm({ admin, role, roles, onCancel, onDone }: {
  admin: Admin; role: PGRole; roles: string[]; onCancel: () => void; onDone: () => void
}) {
  const [typed, setTyped] = useState('')
  const [reassign, setReassign] = useState<string | null>(role.owns > 0 ? 'postgres' : null)
  const [dropOwned, setDropOwned] = useState(true)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<AdminFailure | null>(null)
  const op: AdminOp = { kind: 'role.drop', role: { name: role.name, reassignTo: reassign ?? undefined, dropOwned } }
  const preview = usePreview(admin, op)
  const run = async () => {
    setBusy(true)
    const r = await admin({ ...op })
    setBusy(false)
    if (r.error) { setFailure(r.error); return }
    notifyOk(`${role.name} dropped`, r.tag ?? 'Done')
    onDone()
  }
  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box p="md" maw={640}>
        <Text size={FS.meta}>
          {role.name} {role.owns > 0 ? <>owns <b>{role.owns}</b> table{role.owns === 1 ? '' : 's'}, sequence{role.owns === 1 ? '' : 's'} or function{role.owns === 1 ? '' : 's'} in this database.</> : 'owns nothing in this database.'}
          {role.connections > 0 && <> It has <b>{role.connections}</b> open connection{role.connections === 1 ? '' : 's'} here.</>}
        </Text>
        <Select size="xs" mt="sm" label="Hand what it owns here to" placeholder="Nobody: drop fails if it owns anything" clearable searchable
          data={roles.filter((r) => r !== role.name)} value={reassign} onChange={setReassign} />
        <Checkbox size="xs" mt="sm" checked={dropOwned} onChange={(e) => setDropOwned(e.currentTarget.checked)}
          label="Also remove its privileges here, and anything it still owns (DROP OWNED)" />
        <Text size={FS.micro} c="dimmed" mt={6}>Both act on this database only. If it owns things or holds privileges in another database, Postgres refuses the drop and names them; nothing is changed.</Text>
        <TextInput size="xs" mt="sm" label={`Type ${role.name} to confirm`} value={typed} onChange={(e) => setTyped(e.currentTarget.value)} autoComplete="off" />
        <FormFoot sql={preview.sql} previewError={preview.error} failure={failure} busy={busy} canRun={typed === role.name}
          verb="Drop role" danger onRun={() => void run()} onCancel={onCancel} />
      </Box>
    </ScrollArea>
  )
}

const ON_CHOICES: { value: 'database' | 'schema' | 'table' | 'sequence' | 'function'; label: string }[] = [
  { value: 'database', label: 'This database' },
  { value: 'schema', label: 'A schema' },
  { value: 'table', label: 'Tables & views' },
  { value: 'sequence', label: 'Sequences' },
  { value: 'function', label: 'Functions' },
]

export function GrantForm({ admin, roles, preset, onCancel, onDone }: {
  admin: Admin; roles: PGRole[]; preset: GrantPreset; onCancel: () => void; onDone: (grantee?: string) => void
}) {
  const [revoke, setRevoke] = useState(!!preset.revoke)
  const [grantee, setGrantee] = useState<string | null>(preset.grantee ?? null)
  const presetKind = preset.on ? baseKind(preset.on) : 'table'
  const [kind, setKind] = useState<keyof typeof PRIVILEGES>(presetKind)
  const [every, setEvery] = useState(!preset.on || preset.on.startsWith('all-') || !preset.objects?.length)
  const [schema, setSchema] = useState<string | null>(preset.schema ?? null)
  const [objects, setObjects] = useState<string[]>(preset.objects ?? [])
  const [privs, setPrivs] = useState<string[]>(preset.privileges ?? (presetKind === 'table' ? ['SELECT'] : presetKind === 'database' ? ['CONNECT'] : presetKind === 'schema' ? ['USAGE'] : presetKind === 'function' ? ['EXECUTE'] : ['USAGE', 'SELECT']))
  const [withGrant, setWithGrant] = useState(false)
  const [usage, setUsage] = useState(true)
  const [future, setFuture] = useState(true)
  const [forRoles, setForRoles] = useState<string[] | null>(null)
  const [cascade, setCascade] = useState(false)
  const [catalog, setCatalog] = useState<GrantObjects | null>(null)
  const [busy, setBusy] = useState(false)
  const [failure, setFailure] = useState<AdminFailure | null>(null)

  useEffect(() => {
    let live = true
    void admin<GrantObjects>({ kind: 'grant.objects', grant: schema ? { schema } as AdminOp['grant'] : undefined }).then((r) => {
      if (!live) return
      if (r.data) { setCatalog(r.data); setForRoles((f) => f ?? null) }
    })
    return () => { live = false }
  }, [admin, schema])
  // Whose new objects to cover: the schema's owner and whoever owns what is
  // in it, until the person picks otherwise.
  const owners = catalog?.owners ?? []
  const futureFor = forRoles ?? owners

  const inSchema = kind === 'table' || kind === 'sequence' || kind === 'function'
  const on: On = kind === 'database' || kind === 'schema' ? kind : every ? (`all-${kind}s` as On) : kind
  const all = privs.length === PRIVILEGES[kind].length
  const list = kind === 'table' ? catalog?.tables : kind === 'sequence' ? catalog?.sequences : kind === 'function' ? catalog?.functions : undefined
  const op: AdminOp | null = grantee && privs.length && (kind === 'database' || schema) && (every || !inSchema || objects.length)
    ? { kind: revoke ? 'revoke' : 'grant', grant: {
      grantee, on, schema: kind === 'database' ? undefined : schema ?? undefined,
      objects: inSchema && !every ? objects : undefined,
      privileges: all && PRIVILEGES[kind].length > 1 ? ['ALL'] : privs,
      withGrant: withGrant || undefined,
      schemaUsage: !revoke && inSchema && usage ? true : undefined,
      future: inSchema && every && future && futureFor.length ? true : undefined,
      forRoles: inSchema && every && future && futureFor.length ? futureFor : undefined,
      cascade: revoke && cascade ? true : undefined,
    } as AdminOp['grant'] }
    : null
  const preview = usePreview(admin, op)
  const run = async () => {
    if (!op) return
    setBusy(true)
    const r = await admin({ ...op })
    setBusy(false)
    if (r.error) { setFailure(r.error); return }
    notifyOk(revoke ? 'Revoked' : 'Granted', `${revoke ? 'From' : 'To'} ${grantee}`)
    onDone(grantee ?? undefined)
  }
  const pickKind = (v: string) => {
    const k = v as keyof typeof PRIVILEGES
    setKind(k)
    setPrivs(k === 'table' ? ['SELECT'] : k === 'database' ? ['CONNECT'] : k === 'schema' ? ['USAGE'] : k === 'function' ? ['EXECUTE'] : ['USAGE', 'SELECT'])
    setObjects([])
  }

  return (
    <ScrollArea style={{ flex: 1 }} type="auto">
      <Box p="md" maw={720}>
        <SegmentedControl size="xs" value={revoke ? 'revoke' : 'grant'} onChange={(v) => setRevoke(v === 'revoke')}
          data={[{ value: 'grant', label: 'Grant' }, { value: 'revoke', label: 'Revoke' }]} />
        <Select size="xs" mt="sm" label={revoke ? 'Take from' : 'Give to'} placeholder="Choose a role" searchable w={320}
          data={[...roles.map((r) => r.name), 'PUBLIC']} value={grantee} onChange={setGrantee}
          description={grantee === 'PUBLIC' ? 'PUBLIC is every role on the server, now and later' : 'Application roles; Argus’s own get levels from the database’s settings'} />
        <Text size={FS.meta} fw={600} mt="md">On</Text>
        <SegmentedControl size="xs" mt={4} value={kind} onChange={pickKind} data={ON_CHOICES} fullWidth />
        {kind !== 'database' && (
          <Select size="xs" mt="sm" label="Schema" placeholder="Choose a schema" searchable w={320}
            data={(catalog?.schemas ?? []).map((s) => s.name)} value={schema} onChange={(v) => { setSchema(v); setObjects([]); setForRoles(null) }} />
        )}
        {inSchema && schema && (
          <>
            <SegmentedControl size="xs" mt="sm" value={every ? 'every' : 'pick'} onChange={(v) => setEvery(v === 'every')}
              data={[{ value: 'every', label: `Every one in ${schema}` }, { value: 'pick', label: 'Choose which' }]} />
            {!every && (
              <MultiSelect size="xs" mt="sm" label={`Which ${kind === 'table' ? 'tables and views' : kind + 's'}`} searchable clearable maxDropdownHeight={260}
                data={(list ?? []).map((o) => ({ value: o.name, label: o.kind && o.kind !== kind ? `${o.name} (${o.kind})` : o.name }))}
                value={objects} onChange={setObjects} nothingFoundMessage={list ? 'None' : 'Loading…'} />
            )}
          </>
        )}
        <Text size={FS.meta} fw={600} mt="md">Privileges</Text>
        <Group gap="md" mt={4}>
          {PRIVILEGES[kind].length > 1 && (
            <Checkbox size="xs" label="All" checked={all} indeterminate={!all && privs.length > 0}
              onChange={(e) => setPrivs(e.currentTarget.checked ? [...PRIVILEGES[kind]] : [])} />
          )}
          {PRIVILEGES[kind].map((p) => (
            <Tooltip key={p} label={PRIV_WORDS[p]}>
              <Checkbox size="xs" label={p} checked={privs.includes(p)}
                onChange={(e) => { const c = e.currentTarget.checked; setPrivs((x) => (c ? PRIVILEGES[kind].filter((y) => x.includes(y) || y === p) : x.filter((y) => y !== p))) }} />
            </Tooltip>
          ))}
        </Group>
        <Box mt="md">
          {inSchema && !revoke && (
            <Checkbox size="xs" checked={usage} onChange={(e) => setUsage(e.currentTarget.checked)}
              label={<>Also let it reach the schema (<Code fz={11}>USAGE</Code> on {schema ?? 'it'}); without it, none of these can be used</>} />
          )}
          {inSchema && every && (
            <>
              <Checkbox size="xs" mt={6} checked={future} onChange={(e) => setFuture(e.currentTarget.checked)}
                label={revoke ? `Also stop it for ones made later` : `Also for ${kind === 'table' ? 'tables' : kind + 's'} made later`} />
              {future && schema && (
                <MultiSelect size="xs" mt={6} ml={26} maw={480} label="Made later by" description="Default privileges belong to whoever creates the objects; these own the schema or what is in it now"
                  data={Array.from(new Set([...owners, ...futureFor]))} value={futureFor} onChange={setForRoles} searchable />
              )}
            </>
          )}
          <Checkbox size="xs" mt={6} checked={withGrant} onChange={(e) => setWithGrant(e.currentTarget.checked)}
            label={revoke ? 'Only take away its right to pass these on (GRANT OPTION FOR)' : 'It may pass these on to others (WITH GRANT OPTION)'} />
          {revoke && (
            <Checkbox size="xs" mt={6} checked={cascade} onChange={(e) => setCascade(e.currentTarget.checked)}
              label="Also take them from roles it passed them on to (CASCADE)" />
          )}
        </Box>
        <FormFoot sql={preview.sql} previewError={preview.error} failure={failure} busy={busy} canRun={!!op}
          verb={revoke ? 'Revoke' : 'Grant'} danger={revoke} onRun={() => void run()} onCancel={onCancel} />
      </Box>
    </ScrollArea>
  )
}
