import { useState, type ComponentType } from 'react'
import {
  Alert, Badge, Button, Grid, Group, Modal, NumberInput, PasswordInput, ScrollArea,
  SegmentedControl, Select, Stack, Switch, Table, TagsInput, Text, Textarea, TextInput,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconBell, IconCertificate, IconCloudUpload, IconDatabase,
  IconDownload, IconKey, IconNetwork, IconUserPlus, IconUsers, type IconProps,
} from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { Mono } from '~/components/primitives'
import { PemField } from '~/components/Install'
import { generatePassword } from '~/components/Setup'
import {
  controlSettings, createAccount, saveControlSettings, setAccountDisabled,
  testDatabase, waitForControlPlane, type ControlSettings as Settings,
} from '~/lib/live'
import { usersQuery } from '~/lib/queries'
import { downloadText } from '~/lib/download'
import { notifyError, notifyOk, notifyWarn } from '~/lib/notify'
import { FS } from '~/theme'

const settingsKey = ['control-settings'] as const

export type ControlSectionId =
  | 'addresses' | 'signin' | 'database' | 'certificate' | 'storage' | 'alerts' | 'people'

export interface ControlSectionSpec {
  id: ControlSectionId
  label: string
  icon: ComponentType<IconProps>
  /** The settings this section edits, so the list can mark one with unsaved changes. */
  keys: (keyof Settings)[]
}

export const CONTROL_SECTIONS: ControlSectionSpec[] = [
  { id: 'addresses', label: 'Addresses', icon: IconNetwork,
    keys: ['consoleUrl', 'fleetUrl', 'listen', 'consoleListen', 'quicListen', 'behindTlsProxy', 'allowedOrigins'] },
  { id: 'signin', label: 'Sign-in and limits', icon: IconKey,
    keys: ['sessionTtl', 'ticketTtl', 'agentStaleAfter', 'loginPerMinute', 'failuresPerHour',
      'ticketsPerMinute', 'trustedProxies', 'logLevel'] },
  { id: 'database', label: 'Database', icon: IconDatabase, keys: ['database'] },
  { id: 'certificate', label: 'Certificate', icon: IconCertificate, keys: ['tls'] },
  { id: 'storage', label: 'Object storage', icon: IconCloudUpload, keys: ['storage'] },
  { id: 'alerts', label: 'Session alerts', icon: IconBell, keys: ['email', 'telegram', 'fcm'] },
  { id: 'people', label: 'People', icon: IconUsers, keys: [] },
]

export type ControlSettingsState = ReturnType<typeof useControlSettings>

/**
 * Everything argus-control reads from its configuration file, as one draft.
 *
 * One draft for every section and one save, because the control plane
 * applies a save by restarting on it: seven sections saved one by one would
 * be seven restarts. The draft lives with the page rather than a section, so
 * moving between sections keeps the edits. Secrets are never shown; a field
 * left empty keeps the one configured.
 */
export function useControlSettings(enabled: boolean) {
  const qc = useQueryClient()
  const { data: saved, isPending, error } = useQuery({
    queryKey: settingsKey, queryFn: controlSettings, enabled,
  })
  const [draft, setDraft] = useState<Settings | null>(null)
  const [saving, setSaving] = useState(false)
  const s = draft ?? saved ?? null

  const edit = (patch: Partial<Settings>) => {
    if (!s) return
    setDraft({ ...s, ...patch })
  }

  const changed = (spec: ControlSectionSpec) =>
    Boolean(draft && saved && spec.keys.some((k) => JSON.stringify(draft[k]) !== JSON.stringify(saved[k])))
  const changedSections = CONTROL_SECTIONS.filter(changed)

  const save = async () => {
    if (!draft) return
    setSaving(true)
    try {
      const res = await saveControlSettings(draft)
      if (res.restarting) {
        notifyOk('Settings saved', `Changed ${res.changed.join(', ')}. The control plane is restarting on them.`)
        const up = await waitForControlPlane()
        const now = await qc.fetchQuery({ queryKey: settingsKey, queryFn: controlSettings, staleTime: 0 })
        if (!up) {
          notifyWarn('Still restarting', 'The control plane has not answered yet. Reload this page in a moment.')
        } else if (now.applyError) {
          notifyError('The new settings did not start', new Error(
            `The control plane went back to the previous settings: ${now.applyError}`))
        }
      } else {
        notifyOk('Nothing changed', 'The settings were already as shown.')
      }
      setDraft(null)
      void qc.invalidateQueries({ queryKey: settingsKey })
    } catch (err) {
      notifyError('Settings not saved', err)
    } finally {
      setSaving(false)
    }
  }

  return {
    s, error, loading: enabled && isPending, saving, edit, save,
    discard: () => setDraft(null),
    dirty: changedSections.length > 0,
    changedSections,
  }
}

/** One section of the control plane's settings, by id. */
export function ControlSection({ id, ctl }: { id: ControlSectionId; ctl: ControlSettingsState }) {
  const { s, edit, error } = ctl
  if (error) {
    return (
      <Alert color="amber" icon={<IconAlertTriangle size={16} />} title="Control plane settings">
        <Text size={FS.body}>{error instanceof Error ? error.message : 'Could not read them.'}</Text>
      </Alert>
    )
  }
  if (!s) return null
  const card = { s, edit }
  return (
    <Stack gap="sm">
      {s.applyError && !ctl.dirty && (
        <Alert color="rose" icon={<IconAlertTriangle size={16} />} title="The last save did not take effect" p="xs">
          <Text size={FS.body}>
            The control plane could not start on it and went back to the settings before: {s.applyError}
          </Text>
        </Alert>
      )}
      {id === 'addresses' && <Addresses {...card} />}
      {id === 'signin' && <Sessions {...card} />}
      {id === 'database' && <Database {...card} />}
      {id === 'certificate' && <Certificate {...card} />}
      {id === 'storage' && <Storage {...card} />}
      {id === 'alerts' && <Alerts {...card} />}
      {id === 'people' && <People />}
      {id !== 'people' && (
        <Text size={FS.micro} c="dimmed">
          Saved to <Mono>{s.configFile}</Mono>. Saving restarts the control plane on the new
          settings in a few seconds: people stay signed in and agents reconnect. Settings it
          cannot start on are put back automatically.
        </Text>
      )}
    </Stack>
  )
}

interface CardProps {
  s: Settings
  edit: (patch: Partial<Settings>) => void
}

function Addresses({ s, edit }: CardProps) {
  return (
    <SectionCard title="Addresses" icon={IconNetwork} iconColor="sky"
      description="Where people and servers reach this control plane.">
      <Grid gap="xs">
        <Grid.Col span={{ base: 12, sm: 6 }}>
          <TextInput size="xs" label="Console address" description="Where people open Argus. Alerts link here."
            value={s.consoleUrl} onChange={(e) => edit({ consoleUrl: e.currentTarget.value })} />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 6 }}>
          <TextInput size="xs" label="Address servers use"
            description="Only when it differs from the console address, such as behind a Cloudflare Tunnel."
            value={s.fleetUrl} onChange={(e) => edit({ fleetUrl: e.currentTarget.value })} />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 4 }}>
          <TextInput size="xs" label="Listen" description="Agents, and people unless split."
            value={s.listen} onChange={(e) => edit({ listen: e.currentTarget.value })} />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 4 }}>
          <TextInput size="xs" label="Console listen" description="Optional: people only, elsewhere."
            placeholder="127.0.0.1:8090"
            value={s.consoleListen} onChange={(e) => edit({ consoleListen: e.currentTarget.value })} />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 4 }}>
          <TextInput size="xs" label="QUIC (UDP)" description="Agents' tunnels. Empty turns it off."
            placeholder=":8080"
            value={s.quicListen} onChange={(e) => edit({ quicListen: e.currentTarget.value })} />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 6 }}>
          <TagsInput size="xs" label="Other console origins (CORS)"
            description="Empty is right when the console is served from here."
            value={s.allowedOrigins} onChange={(v) => edit({ allowedOrigins: v })} />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 6 }}>
          <Switch size="xs" mt="lg" checked={s.behindTlsProxy}
            onChange={(e) => edit({ behindTlsProxy: e.currentTarget.checked })}
            label="Something in front terminates TLS"
            description="Only with no certificate here at all. Behind a Cloudflare Tunnel, leave this off." />
        </Grid.Col>
      </Grid>
    </SectionCard>
  )
}

function Sessions({ s, edit }: CardProps) {
  return (
    <SectionCard title="Sign-in and limits" icon={IconKey}
      description="How long sign-ins last, and how hard the sign-in form pushes back. Durations like 8h, 15m or 90s; empty or 0 is the default.">
      <Grid gap="xs">
        <Grid.Col span={{ base: 6, sm: 4 }}>
          <TextInput size="xs" label="Sign-in lasts" placeholder="8h" value={s.sessionTtl}
            onChange={(e) => edit({ sessionTtl: e.currentTarget.value })} />
        </Grid.Col>
        <Grid.Col span={{ base: 6, sm: 4 }}>
          <TextInput size="xs" label="Terminal ticket" placeholder="60s" value={s.ticketTtl}
            onChange={(e) => edit({ ticketTtl: e.currentTarget.value })} />
        </Grid.Col>
        <Grid.Col span={{ base: 6, sm: 4 }}>
          <TextInput size="xs" label="Agent silent after" placeholder="2m" value={s.agentStaleAfter}
            onChange={(e) => edit({ agentStaleAfter: e.currentTarget.value })} />
        </Grid.Col>
        <Grid.Col span={{ base: 6, sm: 4 }}>
          <NumberInput size="xs" label="Sign-ins a minute" min={0} value={s.loginPerMinute}
            onChange={(v) => edit({ loginPerMinute: Number(v) || 0 })} />
        </Grid.Col>
        <Grid.Col span={{ base: 6, sm: 4 }}>
          <NumberInput size="xs" label="Failures an hour" min={0} value={s.failuresPerHour}
            onChange={(v) => edit({ failuresPerHour: Number(v) || 0 })} />
        </Grid.Col>
        <Grid.Col span={{ base: 6, sm: 4 }}>
          <NumberInput size="xs" label="Tickets a minute" min={0} value={s.ticketsPerMinute}
            onChange={(v) => edit({ ticketsPerMinute: Number(v) || 0 })} />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 8 }}>
          <TagsInput size="xs" label="Trusted proxies"
            description="Networks whose forwarding headers are believed: 127.0.0.1/32 and ::1/128 for cloudflared on this host."
            value={s.trustedProxies} onChange={(v) => edit({ trustedProxies: v })} />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 4 }}>
          <Select size="xs" label="Log level" description="What the service logs." allowDeselect={false}
            data={['debug', 'info', 'warn', 'error']} value={s.logLevel || 'info'}
            onChange={(v) => edit({ logLevel: v ?? 'info' })} />
        </Grid.Col>
      </Grid>
    </SectionCard>
  )
}

function Database({ s, edit }: CardProps) {
  const [busy, setBusy] = useState(false)
  const d = s.database
  const set = (patch: Partial<Settings['database']>) => edit({ database: { ...d, ...patch } })
  const test = async () => {
    setBusy(true)
    try {
      const r = await testDatabase(d)
      notifyOk('Database reachable', `PostgreSQL ${r.version.split(' ')[0]}${r.accountsExist ? ', with Argus accounts' : ', with no Argus accounts yet'}.`)
    } catch (err) {
      notifyError('Database not reachable', err)
    } finally {
      setBusy(false)
    }
  }
  return (
    <SectionCard title="Database" icon={IconDatabase} iconColor="teal"
      description="The PostgreSQL this control plane keeps its records in. Changing it is for a new address or password: the records stay in the database they are in.">
      {d.raw ? (
        <Text size={FS.body} c="dimmed">
          The configured connection string is not a URL this page can show in parts, so it is
          left as it is.
        </Text>
      ) : (
        <Stack gap="xs">
          <Grid gap="xs">
            <Grid.Col span={{ base: 12, sm: 8 }}>
              <TextInput size="xs" label="Server" value={d.host} onChange={(e) => set({ host: e.currentTarget.value })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 4 }}>
              <NumberInput size="xs" label="Port" min={1} max={65535} value={d.port || 5432}
                onChange={(v) => set({ port: Number(v) || 0 })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 4 }}>
              <TextInput size="xs" label="Database" value={d.name} onChange={(e) => set({ name: e.currentTarget.value })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 4 }}>
              <TextInput size="xs" label="User" value={d.user} onChange={(e) => set({ user: e.currentTarget.value })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 4 }}>
              <SecretInput label="Password" set={d.passwordSet} value={d.password}
                onChange={(v) => set({ password: v })} />
            </Grid.Col>
          </Grid>
          <Group justify="space-between" align="flex-end">
            <SegmentedControl size="xs" value={d.sslMode || 'require'} onChange={(v) => set({ sslMode: v })}
              data={[
                { value: 'require', label: 'Require TLS' },
                { value: 'verify-full', label: 'Verify' },
                { value: 'disable', label: 'No TLS' },
              ]} />
            <Button size="xs" variant="light" loading={busy} onClick={() => void test()}>Test connection</Button>
          </Group>
        </Stack>
      )}
    </SectionCard>
  )
}

function Certificate({ s, edit }: CardProps) {
  const [opened, modal] = useDisclosure(false)
  const [cert, setCert] = useState('')
  const [key, setKey] = useState('')
  const c = s.tls.certificate
  return (
    <SectionCard title="Certificate" icon={IconCertificate} iconColor="teal"
      badge={s.tls.managed ? <Badge size="xs" variant="light" color="teal">renewed automatically</Badge> : undefined}
      description="What this control plane presents to browsers and agents.">
      <Stack gap="xs">
        {!s.tls.enabled ? (
          <Alert color="amber" icon={<IconAlertTriangle size={16} />} p="xs">
            <Text size={FS.body}>No certificate: the control plane is serving plain HTTP.</Text>
          </Alert>
        ) : c ? (
          <Table fz={FS.body} withRowBorders={false} verticalSpacing={2}>
            <Table.Tbody>
              <Table.Tr><Table.Td c="dimmed" w={110}>Names</Table.Td><Table.Td><Mono>{c.names.join(', ')}</Mono></Table.Td></Table.Tr>
              <Table.Tr><Table.Td c="dimmed">Issued by</Table.Td><Table.Td>{c.issuer}</Table.Td></Table.Tr>
              <Table.Tr><Table.Td c="dimmed">Expires</Table.Td><Table.Td>{new Date(c.notAfter).toLocaleDateString()}</Table.Td></Table.Tr>
            </Table.Tbody>
          </Table>
        ) : null}
        {s.tls.certPem && <Text size={FS.micro} c="amber.4">A new certificate is staged; it is installed when you save.</Text>}
        {s.tls.reissue && <Text size={FS.micro} c="amber.4">A fresh certificate is issued when you save.</Text>}
        <Group gap="xs">
          <Button size="xs" variant="light" leftSection={<IconCloudUpload size={14} />} onClick={modal.open}>
            Upload a certificate
          </Button>
          {s.tls.canIssue && !s.tls.managed && (
            <Button size="xs" variant="subtle" onClick={() => edit({ tls: { ...s.tls, reissue: true, certPem: undefined, keyPem: undefined } })}>
              Use the control plane's own
            </Button>
          )}
          {s.tls.caCert && (
            <Button size="xs" variant="subtle" color="slate" leftSection={<IconDownload size={14} />}
              onClick={() => downloadText(s.tls.caCert!, 'argus-ca.crt', 'application/x-pem-file')}>
              Download the CA
            </Button>
          )}
        </Group>
        {s.tls.caCert && (
          <Text size={FS.micro} c="dimmed">
            Trust the CA in your browsers or operating system and the certificate warning goes
            away.
          </Text>
        )}
      </Stack>
      <Modal opened={opened} onClose={modal.close} title="Upload a certificate" size="lg">
        <Stack gap="sm">
          <Text size={FS.body} c="dimmed">
            From your own CA or a public one. Its names must include every address people and
            agents use to reach this control plane.
          </Text>
          <PemField label="Certificate (PEM, chain included)" value={cert} onChange={setCert} />
          <PemField label="Private key (PEM)" value={key} onChange={setKey} secret />
          <Group justify="flex-end">
            <Button size="xs" disabled={!cert.trim() || !key.trim()} onClick={() => {
              edit({ tls: { ...s.tls, certPem: cert, keyPem: key, reissue: false } })
              modal.close()
            }}>
              Stage it
            </Button>
          </Group>
        </Stack>
      </Modal>
    </SectionCard>
  )
}

function Storage({ s, edit }: CardProps) {
  const st = s.storage
  const set = (patch: Partial<Settings['storage']>) => edit({ storage: { ...st, ...patch } })
  return (
    <SectionCard title="Object storage" icon={IconCloudUpload}
      description="An S3-compatible bucket recordings are copied to, so the evidence is not only on this host. Give Argus a key that can write and not delete.">
      <Stack gap="xs">
        <Switch size="xs" label="Copy recordings to a bucket" checked={st.enabled}
          onChange={(e) => set({ enabled: e.currentTarget.checked })} />
        {st.enabled && (
          <Grid gap="xs">
            <Grid.Col span={{ base: 12, sm: 8 }}>
              <TextInput size="xs" label="Endpoint" placeholder="s3.amazonaws.com" value={st.endpoint}
                onChange={(e) => set({ endpoint: e.currentTarget.value })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 4 }}>
              <TextInput size="xs" label="Region" value={st.region} onChange={(e) => set({ region: e.currentTarget.value })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 6 }}>
              <TextInput size="xs" label="Bucket" value={st.bucket} onChange={(e) => set({ bucket: e.currentTarget.value })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 6 }}>
              <Switch size="xs" mt={22} label="Use HTTPS" checked={st.useSsl}
                onChange={(e) => set({ useSsl: e.currentTarget.checked })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 6 }}>
              <TextInput size="xs" label="Access key" value={st.accessKey} autoComplete="off"
                onChange={(e) => set({ accessKey: e.currentTarget.value })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 6 }}>
              <SecretInput label="Secret key" set={st.secretKeySet} value={st.secretKey}
                onChange={(v) => set({ secretKey: v })} />
            </Grid.Col>
          </Grid>
        )}
      </Stack>
    </SectionCard>
  )
}

/**
 * One channel at a time, so three configured channels do not make the
 * section three screens tall.
 */
function Alerts({ s, edit }: CardProps) {
  const e = s.email
  const t = s.telegram
  const f = s.fcm
  const [channel, setChannel] = useState('email')
  const label = (name: string, on: boolean) => (on ? `${name} · on` : name)
  return (
    <SectionCard title="Session alerts" icon={IconBell} iconColor="amber"
      description="Tell administrators every time somebody opens a session, including logins that went around Argus.">
      <SegmentedControl size="xs" mb="sm" value={channel} onChange={setChannel}
        data={[
          { value: 'email', label: label('Email', e.enabled) },
          { value: 'telegram', label: label('Telegram', t.enabled) },
          { value: 'push', label: label('Push notification', f.enabled) },
        ]} />
      {channel === 'email' && (
        <Stack gap="xs">
          <Switch size="xs" label="Send email" checked={e.enabled}
            onChange={(ev) => edit({ email: { ...e, enabled: ev.currentTarget.checked } })} />
          {e.enabled && (
            <Grid gap="xs">
              <Grid.Col span={{ base: 12, sm: 8 }}>
                <TextInput size="xs" label="SMTP server" value={e.host}
                  onChange={(ev) => edit({ email: { ...e, host: ev.currentTarget.value } })} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 4 }}>
                <NumberInput size="xs" label="Port" description="465 is TLS; others STARTTLS" min={1} max={65535}
                  value={e.port || 587} onChange={(v) => edit({ email: { ...e, port: Number(v) || 0 } })} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 6 }}>
                <TextInput size="xs" label="Username" value={e.username} autoComplete="off"
                  onChange={(ev) => edit({ email: { ...e, username: ev.currentTarget.value } })} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 6 }}>
                <SecretInput label="Password" set={e.passwordSet} value={e.password}
                  onChange={(v) => edit({ email: { ...e, password: v } })} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 6 }}>
                <TextInput size="xs" label="From" value={e.from}
                  onChange={(ev) => edit({ email: { ...e, from: ev.currentTarget.value } })} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 6 }}>
                <TagsInput size="xs" label="To" value={e.to} onChange={(v) => edit({ email: { ...e, to: v } })} />
              </Grid.Col>
            </Grid>
          )}
        </Stack>
      )}
      {channel === 'telegram' && (
        <Stack gap="xs">
          <Switch size="xs" label="Send to Telegram" checked={t.enabled}
            onChange={(ev) => edit({ telegram: { ...t, enabled: ev.currentTarget.checked } })} />
          {t.enabled && (
            <Grid gap="xs">
              <Grid.Col span={{ base: 12, sm: 6 }}>
                <SecretInput label="Bot token" description="From @BotFather" set={t.botTokenSet} value={t.botToken}
                  onChange={(v) => edit({ telegram: { ...t, botToken: v } })} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 6 }}>
                <TagsInput size="xs" label="Chat ids" description="Add the bot to a group first"
                  value={t.chatIds} onChange={(v) => edit({ telegram: { ...t, chatIds: v } })} />
              </Grid.Col>
            </Grid>
          )}
        </Stack>
      )}
      {channel === 'push' && (
        <Stack gap="xs">
          <Switch size="xs" label="Send through Firebase Cloud Messaging" checked={f.enabled}
            onChange={(ev) => edit({ fcm: { ...f, enabled: ev.currentTarget.checked } })} />
          {f.enabled && (
            <Grid gap="xs">
              <Grid.Col span={12}>
                <Textarea size="xs" label="Service account key (JSON)"
                  description={f.credentialsSet && f.credentials === undefined
                    ? 'A key is configured. Paste a new one to replace it.'
                    : 'Firebase console, Project settings, Service accounts.'}
                  autosize minRows={2} maxRows={4} value={f.credentials ?? ''}
                  styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 11 } }}
                  onChange={(ev) => edit({ fcm: { ...f, credentials: ev.currentTarget.value || undefined } })} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 4 }}>
                <TextInput size="xs" label="Topic" value={f.topic}
                  onChange={(ev) => edit({ fcm: { ...f, topic: ev.currentTarget.value } })} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 4 }}>
                <TextInput size="xs" label="Project id" description="Optional" value={f.projectId}
                  onChange={(ev) => edit({ fcm: { ...f, projectId: ev.currentTarget.value } })} />
              </Grid.Col>
              <Grid.Col span={{ base: 12, sm: 4 }}>
                <TagsInput size="xs" label="Device tokens" description="Optional"
                  value={f.tokens} onChange={(v) => edit({ fcm: { ...f, tokens: v } })} />
              </Grid.Col>
            </Grid>
          )}
        </Stack>
      )}
    </SectionCard>
  )
}

/** A write-only secret: empty keeps the configured one. */
function SecretInput({ label, description, set, value, onChange }: {
  label: string
  description?: string
  set: boolean
  value?: string
  onChange: (v: string | undefined) => void
}) {
  return (
    <PasswordInput size="xs" label={label} description={description} autoComplete="new-password"
      placeholder={set ? 'Set. Type to replace it.' : 'Not set'}
      value={value ?? ''} onChange={(e) => onChange(e.currentTarget.value || undefined)} />
  )
}

const ROLES = ['admin', 'approver', 'operator', 'auditor', 'owner']

function People() {
  const qc = useQueryClient()
  const { data: users } = useQuery(usersQuery())
  const [opened, modal] = useDisclosure(false)
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')
  const [role, setRole] = useState('operator')
  const [password, setPassword] = useState('')
  const [busy, setBusy] = useState<string>()

  const add = async () => {
    setBusy('add')
    try {
      await createAccount({ email, displayName: name, role, password })
      notifyOk('Account added', `${email} can sign in as ${role}. Give them the password over a channel you trust; they are asked to enrol a second factor.`)
      setEmail(''); setName(''); setPassword('')
      modal.close()
      void qc.invalidateQueries({ queryKey: ['users'] })
    } catch (err) {
      notifyError('Account not added', err)
    } finally {
      setBusy(undefined)
    }
  }
  const toggle = async (address: string, disabled: boolean) => {
    setBusy(address)
    try {
      await setAccountDisabled(address, disabled)
      notifyOk(disabled ? 'Account disabled' : 'Account enabled', address)
      void qc.invalidateQueries({ queryKey: ['users'] })
    } catch (err) {
      notifyError(disabled ? 'Account not disabled' : 'Account not enabled', err)
    } finally {
      setBusy(undefined)
    }
  }

  return (
    <SectionCard title="People" icon={IconUsers} flush
      description="Who can sign in. Accounts are disabled rather than deleted, because the audit log names them. Changes here take effect at once."
      action={<Button size="xs" variant="light" leftSection={<IconUserPlus size={14} />} onClick={modal.open}>Add</Button>}>
      {/* A long list scrolls inside the card, so the page itself never does. */}
      <ScrollArea.Autosize mah="calc(100dvh - 380px)">
        <Table fz={FS.body} verticalSpacing={4}>
          <Table.Tbody>
            {(users ?? []).map((u) => (
              <Table.Tr key={u.email}>
                <Table.Td>
                  <Text size={FS.body} c={u.disabled ? 'dimmed' : undefined} td={u.disabled ? 'line-through' : undefined}>
                    {u.email}
                  </Text>
                </Table.Td>
                <Table.Td><Badge size="xs" variant="light" color="slate">{u.role}</Badge></Table.Td>
                <Table.Td>{!u.mfaEnrolled && !u.disabled && <Badge size="xs" variant="light" color="amber">no second factor</Badge>}</Table.Td>
                <Table.Td ta="right">
                  <Button size="compact-xs" variant="subtle" color={u.disabled ? 'azure' : 'rose'}
                    loading={busy === u.email} onClick={() => void toggle(u.email, !u.disabled)}>
                    {u.disabled ? 'Enable' : 'Disable'}
                  </Button>
                </Table.Td>
              </Table.Tr>
            ))}
          </Table.Tbody>
        </Table>
      </ScrollArea.Autosize>
      <Modal opened={opened} onClose={modal.close} title="Add an account">
        <Stack gap="sm">
          <TextInput size="sm" label="Email" type="email" value={email} onChange={(e) => setEmail(e.currentTarget.value)} />
          <TextInput size="sm" label="Name" description="Optional" value={name} onChange={(e) => setName(e.currentTarget.value)} />
          <Select size="sm" label="Role" data={ROLES} value={role} allowDeselect={false}
            onChange={(v) => setRole(v ?? 'operator')} />
          <Group align="flex-end" gap="xs" wrap="nowrap">
            <TextInput size="sm" label="First password" description="At least 12 characters" style={{ flex: 1 }}
              value={password} onChange={(e) => setPassword(e.currentTarget.value)}
              styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />
            <Button size="sm" variant="light" color="slate" onClick={() => setPassword(generatePassword())}>Generate</Button>
          </Group>
          <Group justify="flex-end">
            <Button size="sm" loading={busy === 'add'} disabled={!email.includes('@') || [...password].length < 12}
              onClick={() => void add()}>
              Add account
            </Button>
          </Group>
        </Stack>
      </Modal>
    </SectionCard>
  )
}
