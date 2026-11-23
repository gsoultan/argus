import {
  ActionIcon, Alert, Box, Button, Checkbox, Code, CopyButton, Group, NumberInput, PasswordInput, SegmentedControl, Select,
  SimpleGrid, Stack, Text, TextInput, Tooltip,
} from '@mantine/core'
import { IconCheck, IconCopy } from '@tabler/icons-react'
import type { CreateOwner, OwnerConnection, OwnerKind, OwnerUser } from '~/lib/createApi'
import { generatePassword } from '~/lib/password'
import { FS } from '~/theme'
import type { DBEngine } from '~/types/domain'

/*
The owner of a database made from Argus, shared by Create new and the
database's Settings: Argus's own (the default), a user already on the server,
or a new one made in the same step with a password shown once. See
plans/create-db-owner/design.md.

Fields side by side share one label line and one input height; hints go under
the whole row (the layout rule asked for on 2026-11-23).
*/

/** The engines a database owner means something on. */
export const OWNER_ENGINES: DBEngine[] = ['postgres', 'mysql', 'mssql', 'mongodb']
export const hasOwner = (e: DBEngine) => OWNER_ENGINES.includes(e)

const MIN_PASSWORD = 12

/** What each engine calls its users, and Argus's own choice. */
export function ownerWords(engine: DBEngine, host: string) {
  switch (engine) {
    case 'postgres':
      return { argus: 'Argus Owner level', existing: `A role on ${host}`, made: 'A new role', noun: 'role',
        note: 'The owner can create and change anything in the database, and nothing else on the server.' }
    case 'mssql':
      return { argus: 'Argus admin login', existing: `A login on ${host}`, made: 'A new login', noun: 'login',
        note: 'The owner becomes the database’s dbo: full control inside it, nothing outside.' }
    case 'mongodb':
      return { argus: 'No app user', existing: `A user on ${host}`, made: 'A new user', noun: 'user',
        note: 'MongoDB has no owner: the user gets dbOwner on this database only.' }
    default:
      return { argus: 'No app user', existing: `A user on ${host}`, made: 'A new user', noun: 'user',
        note: 'MySQL has no owner: the user gets every privilege on this database only.' }
  }
}

/** The owner form's state. */
export interface OwnerState {
  kind: OwnerKind
  /** The picked existing user, as userKey. */
  picked: string | null
  name: string
  /** True once the person typed a name: it then stops following the database's. */
  named: boolean
  from: string
  policy: boolean
  connLimit: number | string
  pwMode: 'generate' | 'type'
  generated: string
  typed: string
}

const KIND_KEY = 'argus.create.ownerKind'

function rememberedKind(): OwnerKind {
  try {
    const v = localStorage.getItem(KIND_KEY)
    return v === 'existing' || v === 'new' ? v : ''
  } catch {
    return ''
  }
}

export function rememberKind(k: OwnerKind) {
  try { localStorage.setItem(KIND_KEY, k) } catch { /* a private window */ }
}

export function newOwnerState(kind?: OwnerKind): OwnerState {
  return {
    kind: kind ?? rememberedKind(), picked: null, name: '', named: false, from: '%', policy: false, connLimit: '',
    pwMode: 'generate', generated: generatePassword(), typed: '',
  }
}

export const userKey = (u: OwnerUser) => `${u.name}\u0000${u.from ?? ''}`

/** The user name a new owner gets when the person has not typed one. */
export const suggestedOwner = (db: string) => {
  const base = db.trim().toLowerCase().replace(/[^a-z0-9_]/g, '_').replace(/^_+/, '').slice(0, 59)
  return base ? `${base}_app` : ''
}

export function ownerName(s: OwnerState, db: string, users: OwnerUser[]): string {
  if (s.kind === 'new') return s.named ? s.name.trim() : suggestedOwner(db)
  if (s.kind === 'existing') return users.find((u) => userKey(u) === s.picked)?.name ?? ''
  return ''
}

export const ownerPassword = (s: OwnerState) => (s.kind !== 'new' ? '' : s.pwMode === 'generate' ? s.generated : s.typed)

/** Why the password cannot be used, or ''. SQL Server's policy wants three kinds of character. */
export function passwordProblem(s: OwnerState, engine: DBEngine): string {
  if (s.kind !== 'new' || s.pwMode === 'generate') return ''
  const pw = s.typed
  if (pw.length < MIN_PASSWORD) return `At least ${MIN_PASSWORD} characters.`
  if (pw.length > 128) return 'At most 128 characters.'
  // eslint-disable-next-line no-control-regex
  if (/[\u0000-\u001f\u007f]/.test(pw)) return 'No line breaks or control characters.'
  if (engine === 'mssql' && s.policy) {
    const kinds = [/[a-z]/, /[A-Z]/, /[0-9]/, /[^a-zA-Z0-9]/].filter((r) => r.test(pw)).length
    if (kinds < 3) return 'The Windows policy wants three of: lowercase, uppercase, digits, symbols.'
  }
  return ''
}

/** The owner block of a request, or undefined for Argus's. */
export function ownerRequest(s: OwnerState, engine: DBEngine, db: string, users: OwnerUser[]): CreateOwner | undefined {
  if (s.kind === '') return undefined
  const name = ownerName(s, db, users)
  if (s.kind === 'existing') {
    const u = users.find((x) => userKey(x) === s.picked)
    return { kind: 'existing', name, ...(u?.from ? { from: u.from } : {}) }
  }
  return {
    kind: 'new', name,
    ...(engine === 'mysql' ? { from: s.from.trim() || '%' } : {}),
    ...(engine === 'mssql' && s.policy ? { policy: true } : {}),
    ...(engine === 'postgres' && Number(s.connLimit) > 0 ? { connLimit: Number(s.connLimit) } : {}),
  }
}

/** The owner form is complete enough to send. */
export function ownerReady(s: OwnerState, engine: DBEngine, db: string, users: OwnerUser[]): boolean {
  if (s.kind === '') return true
  if (!ownerName(s, db, users)) return false
  return !passwordProblem(s, engine)
}

/** The three choices and what each needs. */
export function OwnerFields({ engine, host, db, users, value, onChange, kinds = ['', 'existing', 'new'], currentOwner }: {
  engine: DBEngine
  host: string
  /** The database's name, for the suggested user name. */
  db: string
  users: OwnerUser[]
  value: OwnerState
  onChange: (s: OwnerState) => void
  kinds?: OwnerKind[]
  /** Argus's Owner level role, shown next to the first choice. */
  currentOwner?: string
}) {
  const w = ownerWords(engine, host)
  const set = (p: Partial<OwnerState>) => onChange({ ...value, ...p })
  const label = { '': w.argus + (currentOwner ? ` (${currentOwner})` : ''), existing: w.existing, new: w.made }
  const pwErr = passwordProblem(value, engine)
  const shownName = value.named ? value.name : suggestedOwner(db)
  return (
    <Stack gap="xs">
      <Stack gap={4}>
        <Text size={FS.meta} fw={500}>Owner</Text>
        <SegmentedControl fullWidth value={value.kind || 'argus'}
          onChange={(v) => { const k = (v === 'argus' ? '' : v) as OwnerKind; rememberKind(k); set({ kind: k }) }}
          data={kinds.map((k) => ({ value: k || 'argus', label: label[k] }))} />
        <Text size={FS.micro} c="dimmed">{w.note}</Text>
      </Stack>

      {value.kind === 'existing' && (
        <Select label={`Which ${w.noun}`} searchable allowDeselect={false} value={value.picked} onChange={(v) => set({ picked: v })}
          placeholder={users.length ? `Pick one of ${users.filter((u) => !u.all).length}` : 'The agent found none'}
          nothingFoundMessage="None by that name"
          data={users.map((u) => ({
            value: userKey(u), disabled: !!u.all,
            label: `${u.name}${engine === 'mysql' && u.from ? `@${u.from}` : engine === 'mongodb' && u.from ? ` (in ${u.from})` : ''}${u.reach ? ` · ${u.reach}` : ''}`,
          }))}
          description={users.some((u) => u.all) ? 'Greyed: it already reaches every database.' : undefined} />
      )}

      {value.kind === 'new' && (
        <Box p="sm" style={{ border: '1px solid var(--color-line)', borderRadius: 8 }}>
          <Stack gap="xs">
            <SimpleGrid cols={engine === 'mongodb' ? 1 : 2} style={{ alignItems: 'end' }}>
              <TextInput label="User name" value={shownName} styles={{ input: { fontFamily: 'var(--font-mono)' }, label: { whiteSpace: 'nowrap' } }}
                onChange={(e) => set({ name: e.currentTarget.value, named: true })} placeholder={suggestedOwner(db) || 'invoices_app'} />
              {engine === 'postgres' && (
                <NumberInput label="Most connections" value={value.connLimit} onChange={(v) => set({ connLimit: v })} min={1} max={10000}
                  placeholder="No limit" styles={{ label: { whiteSpace: 'nowrap' } }} />
              )}
              {engine === 'mysql' && (
                <Select label="Signs in from" value={value.from === '%' || value.from === 'localhost' ? value.from : 'other'} allowDeselect={false}
                  onChange={(v) => set({ from: v === 'other' ? '' : v ?? '%' })} styles={{ label: { whiteSpace: 'nowrap' } }}
                  data={[{ value: '%', label: 'Any machine (%)' }, { value: 'localhost', label: 'Only this server' }, { value: 'other', label: 'An address…' }]} />
              )}
              {engine === 'mssql' && (
                <Checkbox label="Apply the Windows password policy" checked={value.policy} onChange={(e) => set({ policy: e.currentTarget.checked })}
                  mb={8} />
              )}
            </SimpleGrid>
            {engine === 'mysql' && value.from !== '%' && value.from !== 'localhost' && (
              <TextInput label="Address" value={value.from} onChange={(e) => set({ from: e.currentTarget.value })} placeholder="10.20.0.%"
                styles={{ input: { fontFamily: 'var(--font-mono)' } }} />
            )}
            <Text size={FS.micro} c="dimmed">
              {value.named ? '' : 'Suggested from the database’s name. '}It can sign in, owns the database, and nothing else on {host}.
              {engine === 'mysql' ? ' Any machine is still limited by the Network tab and the firewall.' : ''}
            </Text>
            <PasswordField value={value} onChange={onChange} error={pwErr} />
          </Stack>
        </Box>
      )}
    </Stack>
  )
}

/** Generate (the default) or type a password: the Redis users control. */
export function PasswordField({ value, onChange, error }: { value: OwnerState; onChange: (s: OwnerState) => void; error?: string }) {
  const set = (p: Partial<OwnerState>) => onChange({ ...value, ...p })
  return (
    <Stack gap={4}>
      <Text size={FS.meta} fw={500}>Password</Text>
      <Group gap={6} wrap="nowrap" align="center">
        <SegmentedControl size="xs" value={value.pwMode} onChange={(v) => set({ pwMode: v as OwnerState['pwMode'] })}
          data={[{ value: 'generate', label: 'Generate' }, { value: 'type', label: 'Type one' }]} />
        {value.pwMode === 'generate' ? (
          <>
            <Code style={{ flex: 1, minWidth: 0, fontSize: 13, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }}>{value.generated}</Code>
            <CopyIcon value={value.generated} label="Copy password" />
            <Button size="compact-xs" variant="subtle" onClick={() => set({ generated: generatePassword() })}>New</Button>
          </>
        ) : (
          <PasswordInput style={{ flex: 1 }} value={value.typed} onChange={(e) => set({ typed: e.currentTarget.value })} aria-label="Password" />
        )}
      </Group>
      {error ? <Text size={FS.micro} c="rose">{error}</Text> : (
        <Text size={FS.micro} c="dimmed">Shown again with the connection details once it is made. Argus does not keep it: copy it into the app’s settings or your password manager.</Text>
      )}
    </Stack>
  )
}

function CopyIcon({ value, label, onCopy }: { value: string; label: string; onCopy?: () => void }) {
  return (
    <CopyButton value={value}>
      {({ copied, copy }) => (
        <Tooltip label={copied ? 'Copied' : 'Copy'}>
          <ActionIcon variant="default" onClick={() => { copy(); onCopy?.() }} aria-label={label}>
            {copied ? <IconCheck size={14} /> : <IconCopy size={14} />}
          </ActionIcon>
        </Tooltip>
      )}
    </CopyButton>
  )
}

/** A connection string for the app, per engine. */
export function connectionString(engine: DBEngine, c: OwnerConnection, password: string): string {
  const auth = `${encodeURIComponent(c.user)}${password ? `:${encodeURIComponent(password)}` : ''}@${c.host}:${c.port}`
  switch (engine) {
    case 'postgres': return `postgresql://${auth}/${encodeURIComponent(c.database)}`
    case 'mysql': return `mysql://${auth}/${encodeURIComponent(c.database)}`
    case 'mssql': return `sqlserver://${auth}?database=${encodeURIComponent(c.database)}`
    case 'mongodb': return `mongodb://${auth}/${encodeURIComponent(c.database)}?authSource=${encodeURIComponent(c.authSource || c.database)}`
  }
  return ''
}

/** The same, as lines for an app's .env file. */
export function connectionEnv(engine: DBEngine, c: OwnerConnection, password: string): string {
  return [
    `DB_HOST=${c.host}`, `DB_PORT=${c.port}`, `DB_NAME=${c.database}`, `DB_USER=${c.user}`,
    ...(password ? [`DB_PASSWORD=${password}`] : []),
    `DATABASE_URL=${connectionString(engine, c, password)}`,
  ].join('\n')
}

/**
 * Where the owner user connects, with its password, shown this once.
 * onCopied says the person copied something, so closing need not ask.
 */
export function ConnectionDetails({ engine, connection: c, password, onCopied }: {
  engine: DBEngine; connection: OwnerConnection; password: string; onCopied: () => void
}) {
  const row = (label: string, v: string, extra?: string) => (
    <Group gap={8} wrap="nowrap" key={label}>
      <Text size={FS.meta} c="dimmed" w={80} style={{ flexShrink: 0 }}>{label}</Text>
      <Code style={{ flex: 1, minWidth: 0, whiteSpace: 'nowrap', overflow: 'hidden', textOverflow: 'ellipsis' }} title={v}>
        {v}{extra ? <Text span c="dimmed" size={FS.micro}> {extra}</Text> : null}
      </Code>
      <CopyIcon value={v} label={`Copy ${label.toLowerCase()}`} onCopy={onCopied} />
    </Group>
  )
  const str = connectionString(engine, c, password)
  return (
    <Stack gap="xs">
      <Group justify="space-between">
        <Text size={FS.meta} fw={600}>Connection details</Text>
        {password && <Text size={FS.micro} c="rose" fw={600}>shown once</Text>}
      </Group>
      {row('Host', c.host)}
      {row('Port', String(c.port), c.pooled ? 'PgBouncer' : undefined)}
      {row('Database', c.database)}
      {row('User', c.user)}
      {password && row('Password', password)}
      {c.authSource && c.authSource !== c.database && row('Auth source', c.authSource)}
      <Text size={FS.meta} fw={600} mt={4}>Connection string</Text>
      <Code block style={{ whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 11.5 }}>{str}</Code>
      <Group gap="xs">
        <CopyButton value={connectionEnv(engine, c, password)}>
          {({ copied, copy }) => <Button size="compact-sm" variant="default" onClick={() => { copy(); onCopied() }}>{copied ? 'Copied' : 'Copy as .env'}</Button>}
        </CopyButton>
        <CopyButton value={str}>
          {({ copied, copy }) => <Button size="compact-sm" variant="default" onClick={() => { copy(); onCopied() }}>{copied ? 'Copied' : 'Copy string'}</Button>}
        </CopyButton>
      </Group>
      <Text size={FS.micro} c="dimmed">
        {c.pooled ? `PgBouncer ${c.poolMode === 'transaction' || c.poolMode === 'statement' ? `pools by ${c.poolMode} here: session settings and LISTEN do not last between transactions. ` : 'pools by session here. '}Straight to Postgres is port ${c.directPort}. ` : ''}
        {password ? `Argus does not keep the password; if it is lost, give ${c.user} a new one from the database’s Settings.` : ''}
      </Text>
    </Stack>
  )
}

/** Lines the agent could not add itself, to add by hand. */
export function ManualLines({ lines }: { lines: string[] }) {
  if (!lines.length) return null
  return (
    <Alert variant="light" color="amber" p="xs" title="Add these by hand">
      <Stack gap={6}>
        <Text size={FS.meta}>The agent could not change PgBouncer here, so the entry uses Postgres straight for now. Add these lines, reload PgBouncer, then point the entry at PgBouncer in its Connection settings.</Text>
        <Group gap={6} wrap="nowrap" align="flex-start">
          <Code block style={{ flex: 1, minWidth: 0, whiteSpace: 'pre-wrap', wordBreak: 'break-all', fontSize: 11.5 }}>{lines.join('\n')}</Code>
          <CopyIcon value={lines.join('\n')} label="Copy the lines" />
        </Group>
      </Stack>
    </Alert>
  )
}
