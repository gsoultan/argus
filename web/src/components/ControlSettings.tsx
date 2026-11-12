import { useRef, useState, type ComponentType, type ReactNode } from 'react'
import {
  Alert, Badge, Button, Checkbox, Drawer, Grid, Group, Modal, MultiSelect, NumberInput, PasswordInput, Radio,
  SegmentedControl, Select, Stack, Switch, Table, TagsInput, Text, Textarea, TextInput,
} from '@mantine/core'
import { useDisclosure } from '@mantine/hooks'
import { useQuery, useQueryClient } from '@tanstack/react-query'
import {
  IconAlertTriangle, IconArchive, IconBell, IconBrandAws, IconCertificate, IconCloud, IconCloudUpload, IconDatabase,
  IconDownload, IconKey, IconLogin, IconNetwork, IconServer2, IconShieldLock, type IconProps,
} from '@tabler/icons-react'
import { SectionCard } from '~/components/page'
import { Mono } from '~/components/primitives'
import { PemField } from '~/components/Install'
import {
  cloudIdentityStatus, controlSettings, organizations, saveControlSettings,
  testCloudIdentity, testDatabase, testEmail, waitForControlPlane, type AWSIdentity,
  type CloudIdentityCheck, type CloudIdentityStatus, type CloudIdentityUse, type ControlSettings as Settings,
} from '~/lib/live'
import { cloudAccountsQuery } from '~/lib/cloud'
import { downloadText } from '~/lib/download'
import { notifyError, notifyOk, notifyWarn } from '~/lib/notify'
import { FS, INK } from '~/theme'

const settingsKey = ['control-settings'] as const

export type ControlSectionId =
  | 'addresses' | 'signin' | 'security' | 'database' | 'certificate' | 'storage' | 'alerts' | 'cloud'

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
  { id: 'security', label: 'Account security', icon: IconShieldLock, keys: ['security'] },
  { id: 'database', label: 'Database', icon: IconDatabase, keys: ['database'] },
  { id: 'certificate', label: 'Certificate', icon: IconCertificate, keys: ['tls'] },
  { id: 'storage', label: 'Recordings', icon: IconCloudUpload, keys: ['storage', 'agentKeepRecordingsDays', 'archive'] },
  { id: 'alerts', label: 'Alerts', icon: IconBell, keys: ['email', 'telegram', 'fcm', 'signInAlerts', 'serverAlerts'] },
  { id: 'cloud', label: 'Cloud identities', icon: IconCloud, keys: ['cloud'] },
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
      {id === 'security' && <Security {...card} />}
      {id === 'database' && <Database {...card} />}
      {id === 'certificate' && <Certificate {...card} />}
      {id === 'storage' && <Storage {...card} />}
      {id === 'alerts' && <Alerts {...card} />}
      {id === 'cloud' && <CloudIdentities {...card} />}
      <Text size={FS.micro} c="dimmed">
        Saved to <Mono>{s.configFile}</Mono>. Saving restarts the control plane on the new
        settings in a few seconds: people stay signed in and agents reconnect. Settings it
        cannot start on are put back automatically.
      </Text>
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
        {s.tls.certPem && <Text size={FS.micro} c={INK.pending}>A new certificate is staged; it is installed when you save.</Text>}
        {s.tls.reissue && <Text size={FS.micro} c={INK.pending}>A fresh certificate is issued when you save.</Text>}
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

/** What servers keep of a recording the control plane already stores. 0 is the agent's default. */
const KEEP_ON_SERVERS = [
  { value: '1', label: '1 day' },
  { value: '3', label: '3 days' },
  { value: '0', label: '7 days (default)' },
  { value: '14', label: '14 days' },
  { value: '30', label: '30 days' },
  { value: '90', label: '90 days' },
  { value: '-1', label: 'For ever' },
]

/** When a stored recording moves to the archive. 0 is the control plane's default. */
const ARCHIVE_AFTER = [
  { value: '7', label: '7 days' },
  { value: '14', label: '14 days' },
  { value: '0', label: '30 days (default)' },
  { value: '60', label: '60 days' },
  { value: '90', label: '90 days' },
  { value: '180', label: '180 days' },
  { value: '365', label: '1 year' },
  { value: '-1', label: 'Never' },
]

/**
 * Bucket storage classes a recording can be read back from at once. Glacier
 * and Deep Archive are left out on purpose: a recording there cannot be
 * replayed until a restore that takes hours, and the control plane refuses
 * them for the same reason.
 */
const ARCHIVE_CLASSES = [
  { value: '', label: "Bucket's default" },
  { value: 'STANDARD_IA', label: 'Standard-IA' },
  { value: 'ONEZONE_IA', label: 'One Zone-IA' },
  { value: 'INTELLIGENT_TIERING', label: 'Intelligent-Tiering' },
  { value: 'GLACIER_IR', label: 'Glacier Instant Retrieval' },
]

function Storage({ s, edit }: CardProps) {
  const st = s.storage
  const set = (patch: Partial<Settings['storage']>) => edit({ storage: { ...st, ...patch } })
  const keep = String(s.agentKeepRecordingsDays ?? 0)
  const keepOptions = KEEP_ON_SERVERS.some((o) => o.value === keep)
    ? KEEP_ON_SERVERS
    : [...KEEP_ON_SERVERS, { value: keep, label: `${keep} days` }]
  const archive = s.archive ?? { afterDays: 0, storageClass: '' }
  const archiveAfter = String(archive.afterDays)
  const archiveOptions = ARCHIVE_AFTER.some((o) => o.value === archiveAfter)
    ? ARCHIVE_AFTER
    : [...ARCHIVE_AFTER, { value: archiveAfter, label: `${archiveAfter} days` }]
  // A class set in the file that this list does not name is still shown as
  // what it is, rather than as the default it is not.
  const classOptions = ARCHIVE_CLASSES.some((o) => o.value === archive.storageClass)
    ? ARCHIVE_CLASSES
    : [...ARCHIVE_CLASSES, { value: archive.storageClass, label: archive.storageClass }]
  return (
    <>
    <SectionCard title="Copies on servers" icon={IconCloudUpload}
      description="Each server compresses a recording when the session ends and keeps its own copy after this control plane has verified and stored it. A recording this control plane does not hold is never removed from a server.">
      <Grid gap="xs">
        <Grid.Col span={{ base: 12, sm: 6 }}>
          <Select size="xs" label="Keep on servers for" allowDeselect={false}
            data={keepOptions} value={keep}
            onChange={(v) => edit({ agentKeepRecordingsDays: Number(v ?? '0') })} />
        </Grid.Col>
      </Grid>
    </SectionCard>
    {s.archive && (
      <SectionCard title="Archive" icon={IconArchive}
        description="Recordings are stored compressed. Once older than this, each is recompressed at the strongest setting, verified against its chain head and moved to the archive. It replays from the console as before, and every replay still checks it.">
        <Grid gap="xs">
          <Grid.Col span={{ base: 12, sm: 6 }}>
            <Select size="xs" label="Archive recordings older than" allowDeselect={false}
              data={archiveOptions} value={archiveAfter}
              onChange={(v) => edit({ archive: { ...archive, afterDays: Number(v ?? '0') } })} />
          </Grid.Col>
          {st.enabled && (
            <Grid.Col span={{ base: 12, sm: 6 }}>
              <Select size="xs" label="Archive storage class" allowDeselect={false}
                description="Only classes that open at once, so an archived recording still replays."
                data={classOptions} value={archive.storageClass}
                onChange={(v) => edit({ archive: { ...archive, storageClass: v ?? '' } })} />
            </Grid.Col>
          )}
        </Grid>
      </SectionCard>
    )}
    <SectionCard title="Object storage" icon={IconCloudUpload}
      description="An S3-compatible bucket recordings are copied to, so the evidence is not only on this host. Without one, recordings are kept on this host's disk. Give Argus a key that can write and not delete.">
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
    </>
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
  const channels = useRef<HTMLDivElement>(null)
  const label = (name: string, on: boolean) => (on ? `${name} · on` : name)
  // "Set up" from the sign-in card: open the channel, turn it on so its
  // fields show, and bring them into view with the first one focused. Only
  // picking the tab did nothing when that tab was already the open one, and
  // the fields stayed hidden behind the switch.
  const setUp = (name: string) => {
    setChannel(name)
    if (name === 'email' && !e.enabled) edit({ email: { ...e, enabled: true, port: e.port || 587 } })
    if (name === 'telegram' && !t.enabled) edit({ telegram: { ...t, enabled: true } })
    requestAnimationFrame(() => {
      const el = channels.current
      if (!el) return
      el.scrollIntoView({ behavior: 'smooth', block: 'start' })
      el.querySelector<HTMLInputElement>('[data-first-field] input, input[data-first-field]')?.focus({ preventScroll: true })
    })
  }
  // What the control plane needs before it can start on a channel. Listed in
  // one line under the form rather than under each field, so the two columns
  // of a row stay the same height.
  const via = e.via ?? 'smtp'
  const pm = e.panmail ?? { url: '', providerId: '', apiKeySet: false }
  const setPm = (patch: Partial<typeof pm>) => edit({ email: { ...e, panmail: { ...pm, ...patch } } })
  const emailMissing = [
    ...(via === 'panmail'
      ? [!pm.url.trim() && 'Panmail address', !pm.apiKeySet && !pm.apiKey && 'API key', !pm.providerId.trim() && 'Provider id']
      : [!e.host.trim() && 'SMTP server']),
    !e.from.trim() && 'From', e.to.length === 0 && 'To',
  ].filter(Boolean) as string[]
  const [testing, setTesting] = useState(false)
  const sendTest = async () => {
    setTesting(true)
    try {
      const r = await testEmail({ ...e, via, panmail: pm })
      notifyOk('Test email sent', via === 'panmail'
        ? `Panmail queued it for ${r.to.join(', ')}. Check the inbox, then save.`
        : `Sent to ${r.to.join(', ')}. Check the inbox, then save.`)
    } catch (err) {
      notifyError('Test email not sent', err)
    } finally {
      setTesting(false)
    }
  }
  const telegramMissing = [
    !t.botTokenSet && !t.botToken && 'Bot token', t.chatIds.length === 0 && 'Chat ids',
  ].filter(Boolean) as string[]
  return (
    <Stack gap="sm">
      <SignInAlerts s={s} edit={edit} onSetUp={setUp} />
      <ServerAlerts s={s} edit={edit} />
      <div ref={channels} style={{ scrollMarginTop: 72 }}>
        <SectionCard title="Alert channels" icon={IconBell} iconColor="amber"
          description="Where alerts go. Every session opened is announced here, including logins that went around Argus; sign-ins too, as set above. Push notifications carry session alerts only.">
          <SegmentedControl size="xs" mb="sm" value={channel} onChange={setChannel}
            data={[
              { value: 'email', label: label('Email', e.enabled) },
              { value: 'telegram', label: label('Telegram', t.enabled) },
              { value: 'push', label: label('Push notification', f.enabled) },
            ]} />
          {channel === 'email' && (
            <Stack gap="xs">
              <Switch size="xs" label="Send email" checked={e.enabled}
                onChange={(ev) => edit({ email: { ...e, enabled: ev.currentTarget.checked, port: e.port || 587 } })} />
              {!e.enabled && (
                <Text size={FS.meta} c="dimmed">Turn this on to enter your mail server, then save.</Text>
              )}
              {e.enabled && (
                <SegmentedControl size="xs" fullWidth value={via} aria-label="Delivery method"
                  onChange={(v) => edit({ email: { ...e, via: v as 'smtp' | 'panmail', port: e.port || 587, panmail: pm } })}
                  data={[
                    { value: 'smtp', label: 'SMTP server' },
                    { value: 'panmail', label: 'Panmail' },
                  ]} />
              )}
              {e.enabled && via === 'smtp' && (
                <Grid gap="xs">
                  <Grid.Col span={{ base: 12, sm: 8 }}>
                    <TextInput size="xs" label="SMTP server" placeholder="smtp.example.com" data-first-field
                      withAsterisk value={e.host}
                      onChange={(ev) => edit({ email: { ...e, host: ev.currentTarget.value } })} />
                  </Grid.Col>
                  <Grid.Col span={{ base: 12, sm: 4 }}>
                    <NumberInput size="xs" label="Port" min={1} max={65535}
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
                </Grid>
              )}
              {e.enabled && via === 'panmail' && (
                <Grid gap="xs">
                  <Grid.Col span={12}>
                    <TextInput size="xs" label="Panmail address" placeholder="https://mail.example.com"
                      data-first-field withAsterisk value={pm.url} inputMode="url" autoComplete="off"
                      onChange={(ev) => setPm({ url: ev.currentTarget.value })} />
                  </Grid.Col>
                  <Grid.Col span={{ base: 12, sm: 6 }}>
                    <SecretInput label="API key" required set={pm.apiKeySet} value={pm.apiKey}
                      description="Panmail, API Keys. Needs the email:send scope."
                      onChange={(v) => setPm({ apiKey: v })} />
                  </Grid.Col>
                  <Grid.Col span={{ base: 12, sm: 6 }}>
                    <TextInput size="xs" label="Provider id" withAsterisk value={pm.providerId}
                      description="Panmail, Email Providers. The one Argus sends through."
                      placeholder="0f8b8f4e-…" autoComplete="off"
                      styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
                      onChange={(ev) => setPm({ providerId: ev.currentTarget.value })} />
                  </Grid.Col>
                </Grid>
              )}
              {e.enabled && (
                <Grid gap="xs">
                  <Grid.Col span={{ base: 12, sm: 6 }}>
                    <TextInput size="xs" label="From" placeholder="argus@example.com"
                      withAsterisk value={e.from} inputMode="email"
                      description={via === 'panmail' ? 'An allowed sender for that provider.' : undefined}
                      onChange={(ev) => edit({ email: { ...e, from: ev.currentTarget.value } })} />
                  </Grid.Col>
                  <Grid.Col span={{ base: 12, sm: 6 }}>
                    <TagsInput size="xs" label="To" withAsterisk value={e.to}
                      description={via === 'panmail' ? 'Administrators who get every alert.' : undefined}
                      placeholder={e.to.length ? '' : 'Type an address, press Enter'}
                      onChange={(v) => edit({ email: { ...e, to: v } })} />
                  </Grid.Col>
                </Grid>
              )}
              {e.enabled && (
                <Group gap="xs" justify="space-between" align="center" wrap="wrap">
                  <div style={{ flex: '1 1 16rem', minWidth: 0 }}>
                    <FormNote missing={emailMissing}>
                      {via === 'panmail'
                        ? 'Panmail queues each message and delivers it through the provider, with its own retries.'
                        : 'Port 465 connects over TLS; any other port upgrades with STARTTLS.'}
                    </FormNote>
                  </div>
                  <Button size="xs" variant="light" loading={testing} disabled={emailMissing.length > 0}
                    onClick={sendTest}>
                    Send test email
                  </Button>
                </Group>
              )}
            </Stack>
          )}
          {channel === 'telegram' && (
            <Stack gap="xs">
              <Switch size="xs" label="Send to Telegram" checked={t.enabled}
                onChange={(ev) => edit({ telegram: { ...t, enabled: ev.currentTarget.checked } })} />
              {!t.enabled && (
                <Text size={FS.meta} c="dimmed">Turn this on to add a bot token and chats, then save.</Text>
              )}
              {t.enabled && (
                <Grid gap="xs">
                  <Grid.Col span={{ base: 12, sm: 6 }}>
                    <SecretInput label="Bot token" required set={t.botTokenSet} value={t.botToken} first
                      onChange={(v) => edit({ telegram: { ...t, botToken: v } })} />
                  </Grid.Col>
                  <Grid.Col span={{ base: 12, sm: 6 }}>
                    <TagsInput size="xs" label="Chat ids" withAsterisk
                      placeholder={t.chatIds.length ? '' : 'Type a chat id, press Enter'}
                      value={t.chatIds} onChange={(v) => edit({ telegram: { ...t, chatIds: v } })} />
                  </Grid.Col>
                </Grid>
              )}
              {t.enabled && (
                <FormNote missing={telegramMissing}>
                  Get a bot token from @BotFather, add the bot to your group, then add the group's chat id.
                </FormNote>
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
                    <TextInput size="xs" label="Project id (optional)" value={f.projectId}
                      onChange={(ev) => edit({ fcm: { ...f, projectId: ev.currentTarget.value } })} />
                  </Grid.Col>
                  <Grid.Col span={{ base: 12, sm: 4 }}>
                    <TagsInput size="xs" label="Device tokens (optional)"
                      value={f.tokens} onChange={(v) => edit({ fcm: { ...f, tokens: v } })} />
                  </Grid.Col>
                </Grid>
              )}
            </Stack>
          )}
        </SectionCard>
      </div>
    </Stack>
  )
}

/**
 * Who is told when someone signs in to the console. Only through the email
 * and Telegram channels below, and only those that are set up: the switches
 * say so rather than looking on while nothing can be sent.
 */
function SignInAlerts({ s, edit, onSetUp }: CardProps & { onSetUp: (channel: string) => void }) {
  const p = s.signInAlerts ?? { admins: true, person: true, when: 'new_device' as const }
  const set = (patch: Partial<typeof p>) => edit({ signInAlerts: { ...p, ...patch } })
  const email = s.email.enabled
  const telegram = s.telegram.enabled
  const none = !email && !telegram
  const extra = s.email.to.length
  return (
    <SectionCard title="Sign-in alerts" icon={IconLogin} iconColor="amber"
      description="Tell people when someone signs in to the Argus console. Nothing is sent through a channel that isn't set up.">
      <Stack gap="md">
        {none && (
          <Alert color="amber" icon={<IconAlertTriangle size={16} />} p="xs">
            <Text size={FS.body}>Set up email or Telegram below to turn on sign-in alerts. Until then nobody is told.</Text>
          </Alert>
        )}
        <Stack gap={6}>
          <Switch size="xs" label="Tell administrators" disabled={none} checked={p.admins && !none}
            description="Every administrator of the organization, at their own address. For the default organization, also the addresses and chats set up below."
            onChange={(ev) => set({ admins: ev.currentTarget.checked })} />
          <ChannelRow name="Email" on={email} onSetUp={() => onSetUp('email')}
            detail={email
              ? `To each administrator${extra ? ` and ${extra} more address${extra === 1 ? '' : 'es'}` : ''}`
              : "Administrators won't get email until a mail server is added."} />
          <ChannelRow name="Telegram" on={telegram} onSetUp={() => onSetUp('telegram')}
            detail={telegram
              ? `${s.telegram.chatIds.length} chat${s.telegram.chatIds.length === 1 ? '' : 's'}, default organization only`
              : "Administrators won't get Telegram messages until a bot is added."} />
        </Stack>
        <Switch size="xs" label="Tell the person who signed in" disabled={!email} checked={p.person && email}
          description={email
            ? 'By email, to the address on their account, with a "this wasn\'t me" link that signs them out everywhere and tells administrators.'
            : 'By email only. Set up email below to turn this on.'}
          onChange={(ev) => set({ person: ev.currentTarget.checked })} />
        <Radio.Group size="xs" label="When" value={p.when}
          onChange={(v) => set({ when: v === 'every' ? 'every' : 'new_device' })}>
          <Stack gap={6} mt={4}>
            <Radio value="new_device" disabled={none} label="Only a new device or address (recommended)"
              description="A browser or IP address not seen on that account in the last 90 days. Recovery-code sign-ins are always sent." />
            <Radio value="every" disabled={none} label="Every sign-in"
              description="At most one alert per person every 10 minutes." />
          </Stack>
        </Radio.Group>
        <Select size="xs" label="Sign-in location" allowDeselect={false}
          description={LOCATION_HELP[p.location ?? 'proxy']}
          data={[
            { value: 'proxy', label: 'From the proxy in front (Cloudflare), sends nothing anywhere' },
            { value: 'lookup', label: 'Look the address up at ipinfo.io' },
            { value: 'off', label: 'IP address only' },
          ]}
          value={p.location ?? 'proxy'}
          onChange={(v) => set({ location: (v ?? 'proxy') as 'proxy' | 'lookup' | 'off' })} />
      </Stack>
    </SectionCard>
  )
}

const LOCATION_HELP: Record<'proxy' | 'lookup' | 'off', string> = {
  proxy: 'Every alert shows the IP address. Behind Cloudflare, the city and country are added from its visitor location headers, believed only from a trusted proxy (Sign-in and limits).',
  lookup: 'The address of each new sign-in is sent to ipinfo.io to name a city and country, cached for a day. Use only if sending it there is acceptable.',
  off: 'Alerts show the IP address and no place.',
}

/** When administrators are told a server connected. */
function ServerAlerts({ s, edit }: CardProps) {
  const v = s.serverAlerts?.connected ?? 'first'
  const none = !s.email.enabled && !s.telegram.enabled
  return (
    <SectionCard title="Server alerts" icon={IconServer2} iconColor="teal"
      description="Tell administrators, by the email and Telegram set up below, when a server's agent connects. The bell shows every new server either way.">
      <Radio.Group size="xs" value={v}
        onChange={(x) => edit({ serverAlerts: { connected: (x || 'first') as 'first' | 'every' | 'off' } })}>
        <Stack gap={6}>
          <Radio value="first" disabled={none} label="When a server first connects (recommended)"
            description="Once per server, when it is ready to use after its install." />
          <Radio value="every" disabled={none} label="Also when a server comes back"
            description="After it was disconnected. At most once every 30 minutes per server." />
          <Radio value="off" disabled={none} label="Don't send" />
        </Stack>
      </Radio.Group>
    </SectionCard>
  )
}

const SECURITY_DEFAULTS = {
  requireMfa: 'all' as const, temporaryPasswordTtl: '', allowBrowserChange: false,
  accountFailuresPerHour: 0, accountCodeFailuresPerHour: 0,
}

/** How sign-in is protected: two-step, temporary passwords, copied cookies, lockout. */
function Security({ s, edit }: CardProps) {
  const p = s.security ?? SECURITY_DEFAULTS
  const set = (patch: Partial<typeof p>) => edit({ security: { ...p, ...patch } })
  return (
    <SectionCard title="Account security" icon={IconShieldLock} iconColor="teal"
      description="How people sign in to the console. Changes apply to everyone at their next request.">
      <Grid gap="xs">
        <Grid.Col span={12}>
          <Select size="xs" label="Two-step sign-in" allowDeselect={false}
            description={p.requireMfa === 'off'
              ? 'Nobody is made to set it up. One phished password is then every server this console reaches.'
              : 'Whoever has to set it up cannot do anything else until they have.'}
            data={[
              { value: 'all', label: 'Required for everyone (recommended)' },
              { value: 'privileged', label: 'Required for owners, admins and approvers' },
              { value: 'off', label: 'Not required' },
            ]}
            value={p.requireMfa}
            onChange={(v) => set({ requireMfa: (v ?? 'all') as 'all' | 'privileged' | 'off' })} />
          {p.requireMfa === 'off' && (
            <Alert color="rose" variant="light" icon={<IconAlertTriangle size={16} />} p="xs" mt="xs">
              <Text size={FS.body}>Without two-step sign-in, a leaked password is enough to open shells on your servers.</Text>
            </Alert>
          )}
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 4 }}>
          <TextInput size="xs" label="Temporary password lasts" placeholder="72h"
            description="Sent to people you add. Unused, it stops working after this."
            value={p.temporaryPasswordTtl}
            onChange={(e) => set({ temporaryPasswordTtl: e.currentTarget.value })} />
        </Grid.Col>
        <Grid.Col span={{ base: 6, sm: 4 }}>
          <NumberInput size="xs" label="Wrong passwords an hour" placeholder="10" min={0}
            description="Per person, from every address. Browsers they used before are not held back."
            value={p.accountFailuresPerHour || ''}
            onChange={(v) => set({ accountFailuresPerHour: Number(v) || 0 })} />
        </Grid.Col>
        <Grid.Col span={{ base: 6, sm: 4 }}>
          <NumberInput size="xs" label="Wrong codes an hour" placeholder="5" min={0}
            description="Per person, for two-step codes. No exemptions."
            value={p.accountCodeFailuresPerHour || ''}
            onChange={(v) => set({ accountCodeFailuresPerHour: Number(v) || 0 })} />
        </Grid.Col>
        <Grid.Col span={12}>
          <Switch size="xs" checked={!p.allowBrowserChange}
            onChange={(e) => set({ allowBrowserChange: !e.currentTarget.checked })}
            label="Sign out a session used from a different browser (recommended)"
            description="A sign-in works only in the browser that made it. If its cookie turns up in another browser or a script, Argus refuses it, signs that session out and tells administrators. A browser updating itself is fine." />
        </Grid.Col>
      </Grid>
    </SectionCard>
  )
}

/** What the IAM user behind an access key needs: to assume Argus's roles, and nothing else. */
const AWS_BASE_POLICY = `{
  "Version": "2012-10-17",
  "Statement": [{
    "Effect": "Allow",
    "Action": ["sts:AssumeRole", "sts:SetSourceIdentity", "sts:TagSession"],
    "Resource": "arn:aws:iam::*:role/Argus*"
  }]
}`

const AWS_SOURCES: { value: AWSIdentity['source']; label: string; detail: string }[] = [
  { value: 'default', label: 'From where argus-control runs',
    detail: 'An EC2 instance profile, a container task role, or AWS_ environment variables set for the service.' },
  { value: 'key', label: 'An access key',
    detail: 'For a control plane that is not on AWS. Make an IAM user that can only assume the Argus roles, and give it one access key.' },
  { value: 'file', label: 'A profile or credentials file',
    detail: 'A named profile on this host, for example IAM Roles Anywhere through credential_process, or a credentials file only argus-control can read.' },
]

/** One identity's key in the draft: its id, or its name while it has none. */
const identityKey = (i: AWSIdentity) => i.id || `new:${i.name}`

/** Where an identity comes from, in a few words, with nothing secret. */
function identitySource(i: AWSIdentity): string {
  if (i.source === 'key') return i.accessKeyId ? `Access key ${i.accessKeyId.slice(0, 4)}…${i.accessKeyId.slice(-4)}` : 'Access key'
  if (i.source === 'file') return [i.profile && `Profile ${i.profile}`, i.credentialsFile].filter(Boolean).join(' · ') || 'Profile'
  return 'Where argus-control runs'
}

const BLANK_IDENTITY: AWSIdentity = {
  id: '', name: '', organizations: [], source: 'key', accessKeyId: '', secretAccessKeySet: false,
  profile: '', credentialsFile: '', homeRegion: '',
}

/**
 * Who argus-control is to each cloud: a list of identities, one of them the
 * default a new connection starts with. Each connected account keeps the
 * identity it was connected with until someone moves it, so changing the
 * default moves nothing, and an identity accounts still use cannot be
 * removed. Shared by the platform: only the default organization edits
 * them; every other one sees the ones it may use, read only.
 */
function CloudIdentities({ s, edit }: CardProps) {
  const platform = Boolean(s.cloud)
  const status = useQuery({
    queryKey: ['cloud-identity-status', 'aws'], queryFn: () => cloudIdentityStatus('aws'), enabled: platform, retry: false,
  })
  const orgs = useQuery({ queryKey: ['organizations'], queryFn: organizations, enabled: platform, retry: false })
  const [editing, setEditing] = useState<{ key: string | null; draft: AWSIdentity; makeDefault: boolean } | null>(null)
  const [removing, setRemoving] = useState<AWSIdentity | null>(null)
  const [checks, setChecks] = useState<Record<string, CloudIdentityCheck | Error | 'busy'>>({})
  if (!s.cloud) return <SharedIdentities />

  const aws = s.cloud.aws
  const list = aws.identities
  const orgName = (id: string) => orgs.data?.find((o) => o.id === id)?.name ?? (id === 'default' ? 'Default' : id)
  const statusOf = (i: AWSIdentity) => (i.id ? status.data?.find((x) => x.identity === i.id) : undefined)
  const usedBy = (i: AWSIdentity) => statusOf(i)?.use ?? []
  const used = (i: AWSIdentity) => usedBy(i).reduce((n, u) => n + u.accounts, 0)
  const isDefault = (i: AWSIdentity) => aws.default === i.id || (!i.id && aws.default === i.name)
  const save = (identities: AWSIdentity[], def = aws.default) => edit({ cloud: { aws: { default: def, identities } } })

  const test = async (i: AWSIdentity) => {
    const k = identityKey(i)
    setChecks((c) => ({ ...c, [k]: 'busy' }))
    try {
      const r = await testCloudIdentity('aws', i)
      setChecks((c) => ({ ...c, [k]: r }))
      return r
    } catch (err) {
      setChecks((c) => ({ ...c, [k]: err instanceof Error ? err : new Error(String(err)) }))
      return null
    }
  }
  const testAll = async () => {
    // Four at a time, each bounded by the control plane's own timeout.
    const queue = [...list]
    const worker = async () => { for (let i = queue.shift(); i; i = queue.shift()) await test(i) }
    await Promise.all([worker(), worker(), worker(), worker()])
    notifyOk('Tested', `Asked AWS about ${list.length} identit${list.length === 1 ? 'y' : 'ies'}.`)
  }
  const done = (d: AWSIdentity, makeDefault: boolean, key: string | null) => {
    const next = key === null ? [...list, d] : list.map((i) => (identityKey(i) === key ? d : i))
    const def = makeDefault ? d.id || d.name : aws.default === key?.replace(/^new:/, '') && !d.id ? d.name : aws.default
    save(next, def)
    setEditing(null)
  }
  const hosts = list.filter((i) => i.source === 'default').length

  return (
    <SectionCard title="Cloud identities" icon={IconCloud}
      description="Who argus-control is to AWS before it steps into the role in a connected account. Each account keeps the identity it was connected with until you move it. An organization's accounts trust an identity only together with an external id made for that organization and account, so sharing one never lets an organization reach another's accounts."
      action={(
        <Group gap={6}>
          <Button size="xs" variant="default" onClick={() => void testAll()}>Test all</Button>
          <Button size="xs" disabled={list.length >= 20}
            onClick={() => setEditing({ key: null, makeDefault: false, draft: { ...BLANK_IDENTITY, source: hosts ? 'key' : 'default' } })}>
            Add identity
          </Button>
        </Group>
      )}>
      <Stack gap="sm">
        <Group gap="xs"><IconBrandAws size={18} /><Text fw={600} size={FS.body}>AWS</Text></Group>
        <Table.ScrollContainer minWidth={760}>
          <Table verticalSpacing="xs" fz={FS.meta}>
            <Table.Thead>
              <Table.Tr>
                <Table.Th>Identity</Table.Th>
                <Table.Th>Accounts' roles trust</Table.Th>
                <Table.Th>Available to</Table.Th>
                <Table.Th>Used by</Table.Th>
                <Table.Th>Status</Table.Th>
                <Table.Th />
              </Table.Tr>
            </Table.Thead>
            <Table.Tbody>
              {list.map((i) => {
                const k = identityKey(i)
                const st = statusOf(i)
                const chk = checks[k]
                const principal = chk && chk !== 'busy' && !(chk instanceof Error) ? chk.principal : st?.principal
                const n = used(i)
                return (
                  <Table.Tr key={k}>
                    <Table.Td>
                      <Group gap={6} wrap="nowrap">
                        <Text fw={600} size={FS.body}>{i.name}</Text>
                        {isDefault(i) && <Badge size="sm" variant="light" color="azure" tt="none">Default</Badge>}
                        {!i.id && <Badge size="sm" variant="light" color="amber" tt="none">Not saved</Badge>}
                      </Group>
                      <Text size={FS.micro} c="dimmed">{identitySource(i)}{i.homeRegion ? ` · ${i.homeRegion}` : ''}</Text>
                    </Table.Td>
                    <Table.Td style={{ maxWidth: 300 }}>
                      {principal ? <Text size={FS.micro} ff="monospace" style={{ overflowWrap: 'anywhere' }}>{principal}</Text>
                        : <Text size={FS.micro} c="dimmed">Test to see</Text>}
                      {chk instanceof Error ? <Text size={FS.micro} c={INK.denied}>{chk.message}</Text>
                        : !chk && st?.error ? <Text size={FS.micro} c={INK.denied}>{st.error}</Text> : null}
                    </Table.Td>
                    <Table.Td>
                      {i.organizations.length === 0 ? <Text size={FS.meta}>All organizations</Text> : (
                        <Group gap={4}>
                          {i.organizations.map((o) => <Badge key={o} size="sm" variant="default" tt="none" fw={500}>{orgName(o)}</Badge>)}
                        </Group>
                      )}
                    </Table.Td>
                    <Table.Td>
                      <Text size={FS.meta}>
                        {!i.id ? '—' : n === 0 ? 'Not used yet'
                          : `${n} account${n === 1 ? '' : 's'} · ${usedBy(i).length === 1 ? usedBy(i)[0]?.name : `${usedBy(i).length} organizations`}`}
                      </Text>
                    </Table.Td>
                    <Table.Td>
                      {chk === 'busy' ? <Badge size="sm" variant="light" color="slate" tt="none">Asking…</Badge>
                        : chk instanceof Error ? <Badge size="sm" variant="light" color="rose" tt="none">No answer</Badge>
                          : chk ? <Badge size="sm" variant="light" color="teal" tt="none">Working</Badge>
                            : !i.id ? <Badge size="sm" variant="light" color="slate" tt="none">Not checked yet</Badge>
                              : st?.principal ? <Badge size="sm" variant="light" color="teal" tt="none">Working</Badge>
                                : st?.error ? <Badge size="sm" variant="light" color="rose" tt="none">No answer</Badge>
                                  : <Badge size="sm" variant="light" color="slate" tt="none">Not checked yet</Badge>}
                    </Table.Td>
                    <Table.Td>
                      <Group gap={2} wrap="nowrap" justify="flex-end">
                        <Button size="compact-xs" variant="default" loading={chk === 'busy'} onClick={() => void test(i)}>Test</Button>
                        <Button size="compact-xs" variant="subtle" onClick={() => setEditing({ key: k, draft: { ...i }, makeDefault: isDefault(i) })}>Edit</Button>
                        <Button size="compact-xs" variant="subtle" color="rose" onClick={() => setRemoving(i)}>Remove</Button>
                      </Group>
                    </Table.Td>
                  </Table.Tr>
                )
              })}
            </Table.Tbody>
          </Table>
        </Table.ScrollContainer>
        <Group gap="xs" align="center">
          <Text size={FS.meta} c="dimmed">New connections start with</Text>
          <Select size="xs" w={240} allowDeselect={false} value={aws.default}
            data={list.map((i) => ({ value: i.id || i.name, label: i.name }))}
            onChange={(v) => v && save(list, v)} aria-label="Default identity" />
          <Text size={FS.meta} c="dimmed">Changing this never moves an account that is already connected.</Text>
        </Group>
        <Text size={FS.micro} c="dimmed">
          Each access key's secret is its own owner-only file in the secrets directory. Saving restarts argus-control on the
          identities. Only AWS can be connected so far.
        </Text>
      </Stack>
      {editing && (
        <IdentityDrawer key={editing.key ?? 'new'} start={editing.draft} makeDefault={editing.makeDefault} adding={editing.key === null}
          others={list.filter((i) => identityKey(i) !== editing.key)} orgs={orgs.data ?? null}
          used={editing.key ? used(list.find((i) => identityKey(i) === editing.key) ?? BLANK_IDENTITY) : 0}
          running={editing.key ? statusOf(list.find((i) => identityKey(i) === editing.key) ?? BLANK_IDENTITY) : undefined}
          onClose={() => setEditing(null)} onDone={(d, def) => done(d, def, editing.key)} />
      )}
      {removing && (
        <RemoveIdentity identity={removing} isDefault={isDefault(removing)} use={usedBy(removing)}
          onClose={() => setRemoving(null)}
          onRemove={() => { save(list.filter((i) => identityKey(i) !== identityKey(removing))); setRemoving(null) }} />
      )}
    </SectionCard>
  )
}

/** Adding or changing one identity. Nothing is saved until Save and apply. */
function IdentityDrawer({ start, makeDefault, adding, others, orgs, used, running, onClose, onDone }: {
  start: AWSIdentity
  makeDefault: boolean
  adding: boolean
  others: AWSIdentity[]
  orgs: { id: string; name: string }[] | null
  used: number
  running?: CloudIdentityStatus
  onClose: () => void
  onDone: (d: AWSIdentity, makeDefault: boolean) => void
}) {
  const [d, setD] = useState(start)
  const [def, setDef] = useState(makeDefault)
  const [only, setOnly] = useState(start.organizations.length > 0)
  const [busy, setBusy] = useState(false)
  const [checked, setChecked] = useState<CloudIdentityCheck | null>(null)
  const set = (patch: Partial<AWSIdentity>) => { setChecked(null); setD((x) => ({ ...x, ...patch })) }
  const hostTaken = others.some((i) => i.source === 'default')
  const nameTaken = others.some((i) => i.name.trim().toLowerCase() === d.name.trim().toLowerCase())
  const missing = [
    !d.name.trim() && 'a name',
    nameTaken && 'a name no other identity has',
    d.source === 'key' && !d.accessKeyId.trim() && 'access key id',
    d.source === 'key' && !d.secretAccessKeySet && !d.secretAccessKey && 'secret access key',
    d.source === 'file' && !d.profile.trim() && !d.credentialsFile.trim() && 'a profile or a credentials file',
    d.source === 'default' && hostTaken && 'another source: the host already gives one identity',
    only && d.organizations.length === 0 && 'at least one organization',
  ].filter((m): m is string => Boolean(m))
  const test = async () => {
    setBusy(true)
    setChecked(null)
    try {
      const r = await testCloudIdentity('aws', d)
      setChecked(r)
    } catch (err) {
      notifyError('AWS did not accept it', err)
    } finally {
      setBusy(false)
    }
  }
  // A new principal is one the accounts on this identity do not trust yet.
  const moves = Boolean(checked && running?.principal && checked.principal !== running.principal && used > 0)
  return (
    <Drawer opened onClose={onClose} position="right" size="lg" title={<Text fw={700}>{adding ? 'Add an AWS identity' : `Change ${start.name}`}</Text>}>
      <Stack gap="sm">
        <Text size={FS.meta} c="dimmed">Accounts connected with it will trust the principal AWS reports for it.</Text>
        <TextInput size="xs" label="Name" withAsterisk description="Shown to every organization that may use it." maxLength={64}
          value={d.name} onChange={(e) => set({ name: e.currentTarget.value })} error={nameTaken ? 'Another identity has this name' : undefined} data-autofocus />
        <Radio.Group label="Where it comes from" value={d.source} onChange={(v) => set({ source: v as AWSIdentity['source'] })}>
          <Stack gap="xs" mt={4}>
            {AWS_SOURCES.map((o) => (
              <Radio key={o.value} size="xs" value={o.value} label={o.label}
                disabled={o.value === 'default' && hostTaken}
                description={o.value === 'default' && hostTaken ? 'Another identity already comes from the host; it has only one.' : o.detail} />
            ))}
          </Stack>
        </Radio.Group>
        {d.source === 'key' && (
          <Grid gap="xs">
            <Grid.Col span={{ base: 12, sm: 6 }}>
              <TextInput size="xs" label="Access key id" placeholder="AKIA…" autoComplete="off" withAsterisk
                value={d.accessKeyId} onChange={(e) => set({ accessKeyId: e.currentTarget.value })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 6 }}>
              <SecretInput label="Secret access key" set={d.secretAccessKeySet} value={d.secretAccessKey} required
                onChange={(v) => set({ secretAccessKey: v })} />
            </Grid.Col>
            <Grid.Col span={12}>
              <Text size={FS.meta} c="dimmed" mb={4}>
                The IAM user needs this policy and nothing more. Each account&apos;s argus-access stack decides what the role can do there.
              </Text>
              <Textarea size="xs" readOnly autosize minRows={4} value={AWS_BASE_POLICY}
                styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }} />
            </Grid.Col>
          </Grid>
        )}
        {d.source === 'file' && (
          <Grid gap="xs">
            <Grid.Col span={{ base: 12, sm: 6 }}>
              <TextInput size="xs" label="Profile" placeholder="argus" value={d.profile}
                onChange={(e) => set({ profile: e.currentTarget.value })} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 6 }}>
              <TextInput size="xs" label="Credentials file" placeholder="/var/lib/argus-control/aws-credentials"
                description="Mode 600, owned by the argus user." value={d.credentialsFile}
                onChange={(e) => set({ credentialsFile: e.currentTarget.value })} />
            </Grid.Col>
          </Grid>
        )}
        <TextInput size="xs" label="Home region" placeholder="us-east-1" w={{ base: '100%', sm: '50%' }}
          description="Where STS is asked. Change it only for another partition, such as GovCloud."
          value={d.homeRegion} onChange={(e) => set({ homeRegion: e.currentTarget.value })} />
        <Radio.Group label="Available to" value={only ? 'only' : 'all'}
          onChange={(v) => { setOnly(v === 'only'); if (v === 'all') set({ organizations: [] }) }}>
          <Stack gap="xs" mt={4}>
            <Radio size="xs" value="all" label="All organizations" />
            <Radio size="xs" value="only" label="Only these organizations" />
          </Stack>
        </Radio.Group>
        {only && (orgs ? (
          <MultiSelect size="xs" placeholder="Pick organizations" searchable value={d.organizations}
            data={orgs.map((o) => ({ value: o.id, label: o.name }))} onChange={(v) => set({ organizations: v })} />
        ) : (
          <TagsInput size="xs" placeholder="Organization ids" description="Only the platform's owner can list organizations; type their ids."
            value={d.organizations} onChange={(v) => set({ organizations: v })} />
        ))}
        <Checkbox size="xs" label="New connections start with it" checked={def} onChange={(e) => setDef(e.currentTarget.checked)} />
        {checked && (
          <Alert color={moves ? 'amber' : 'teal'} variant="light" p="xs"
            icon={moves ? <IconAlertTriangle size={16} /> : undefined}
            title={moves ? `The ${used} accounts on ${start.name} do not trust this yet` : 'AWS knows this identity'}>
            <Text size={FS.meta} style={{ overflowWrap: 'anywhere' }}>
              It answers as <Mono>{checked.arn}</Mono>, from {checked.source}.
              {moves
                ? ' After saving, Argus stops reaching them until their stacks trust it. To change without a gap, add this as a new identity and move the accounts to it one by one with Change identity under Cloud.'
                : <> Accounts connected with it will trust <Mono>{checked.principal}</Mono>.</>}
            </Text>
          </Alert>
        )}
        <Group justify="space-between" align="center" mt="xs">
          <FormNote missing={missing}>Test asks AWS who this is without saving it.</FormNote>
          <Group gap="xs">
            <Button size="xs" variant="default" loading={busy} disabled={missing.length > 0} onClick={() => void test()}>Test</Button>
            <Button size="xs" disabled={missing.length > 0} onClick={() => onDone({ ...d, name: d.name.trim() }, def)}>{adding ? 'Add' : 'Done'}</Button>
          </Group>
        </Group>
        <Text size={FS.micro} c="dimmed">{adding ? 'Add' : 'Done'} puts it in the list; Save and apply writes it and restarts argus-control.</Text>
      </Stack>
    </Drawer>
  )
}

/** Removing an identity: refused while accounts use it or while it is the default. */
function RemoveIdentity({ identity, isDefault, use, onClose, onRemove }: {
  identity: AWSIdentity
  isDefault: boolean
  use: CloudIdentityUse[]
  onClose: () => void
  onRemove: () => void
}) {
  const n = use.reduce((s, u) => s + u.accounts, 0)
  const blocked = n > 0 || isDefault
  return (
    <Modal opened onClose={onClose} centered size="lg"
      title={<Text fw={700}>{n > 0 ? `${identity.name} is still used by ${n} account${n === 1 ? '' : 's'}` : `Remove ${identity.name}?`}</Text>}>
      <Stack gap="md">
        {n > 0 ? (
          <>
            <Text size={FS.body} lh={1.55}>
              Argus would lose those accounts, so it cannot be removed yet. Move them to another identity first, with
              <b> Change identity</b> on each account&apos;s row under Cloud in its organization. Argus moves an account only once it
              gets in with the new identity, so nothing stops working.
            </Text>
            <Table fz={FS.meta} withTableBorder>
              <Table.Thead><Table.Tr><Table.Th>Organization</Table.Th><Table.Th>Accounts on it</Table.Th></Table.Tr></Table.Thead>
              <Table.Tbody>
                {use.map((u) => <Table.Tr key={u.organization}><Table.Td>{u.name}</Table.Td><Table.Td>{u.accounts}</Table.Td></Table.Tr>)}
              </Table.Tbody>
            </Table>
          </>
        ) : isDefault ? (
          <Text size={FS.body}>New connections start with it. Pick another default first.</Text>
        ) : (
          <Text size={FS.body} lh={1.55}>
            No account uses it. Saving removes it{identity.source === 'key' ? ' and deletes its secret access key from this host' : ''}.
          </Text>
        )}
        {n > 0 && isDefault && <Text size={FS.meta} c="dimmed">It is also the default: pick another default too.</Text>}
        <Group justify="flex-end">
          <Button variant="default" onClick={onClose}>{blocked ? 'Close' : 'Cancel'}</Button>
          {!blocked && <Button color="rose" onClick={onRemove}>Remove</Button>}
        </Group>
      </Stack>
    </Modal>
  )
}

/**
 * Outside the default organization: the identities this organization may
 * use, read only, with nothing about where they come from.
 */
function SharedIdentities() {
  const accounts = useQuery(cloudAccountsQuery())
  const bases = accounts.data?.bases ?? []
  const count = (id: string) => accounts.data?.accounts.filter((a) => a.identity === id).length ?? 0
  return (
    <SectionCard title="Cloud identities" icon={IconCloud}
      description="The identities this organization's accounts can trust. The platform's owner adds and changes them from the default organization; their keys never leave the control host.">
      <Stack gap="sm">
        {accounts.isPending ? <Text size={FS.meta} c="dimmed">Loading…</Text> : bases.length === 0 ? (
          <Text size={FS.body} c="dimmed">
            No AWS identity is available to this organization yet. Ask the platform&apos;s owner to make one available from the
            default organization&apos;s Settings › Cloud identities.
          </Text>
        ) : (
          <Table.ScrollContainer minWidth={560}>
            <Table verticalSpacing="xs" fz={FS.meta}>
              <Table.Thead>
                <Table.Tr><Table.Th>Identity</Table.Th><Table.Th>Your accounts' roles trust</Table.Th><Table.Th>Used by</Table.Th><Table.Th>Status</Table.Th></Table.Tr>
              </Table.Thead>
              <Table.Tbody>
                {bases.map((b) => {
                  const n = count(b.identity)
                  return (
                    <Table.Tr key={b.identity}>
                      <Table.Td>
                        <Group gap={6} wrap="nowrap">
                          <Text fw={600} size={FS.body}>{b.name}</Text>
                          {b.default && <Badge size="sm" variant="light" color="azure" tt="none">Default</Badge>}
                        </Group>
                      </Table.Td>
                      <Table.Td style={{ maxWidth: 320 }}>
                        {b.principal ? <Text size={FS.micro} ff="monospace" style={{ overflowWrap: 'anywhere' }}>{b.principal}</Text>
                          : <Text size={FS.micro} c={INK.denied}>{b.error}</Text>}
                      </Table.Td>
                      <Table.Td>{n === 0 ? 'Not used yet' : `${n} of your accounts`}</Table.Td>
                      <Table.Td>
                        {b.principal ? <Badge size="sm" variant="light" color="teal" tt="none">Working</Badge>
                          : <Badge size="sm" variant="light" color="rose" tt="none">No answer</Badge>}
                      </Table.Td>
                    </Table.Tr>
                  )
                })}
              </Table.Tbody>
            </Table>
          </Table.ScrollContainer>
        )}
        <Text size={FS.meta} c="dimmed">To move one of your accounts to another identity, use Change identity on its row under Cloud.</Text>
      </Stack>
    </SectionCard>
  )
}

function ChannelRow({ name, on, detail, onSetUp }: {
  name: string
  on: boolean
  detail: string
  onSetUp: () => void
}) {
  return (
    <Group gap="xs" wrap="nowrap" pl="xl">
      <Badge size="sm" variant="light" color={on ? 'teal' : 'slate'}>{on ? 'Set up' : 'Not set up'}</Badge>
      <Text size={FS.meta} fw={600}>{name}</Text>
      <Text size={FS.meta} c="dimmed" style={{ flex: 1 }}>{detail}</Text>
      {!on && <Button size="compact-xs" variant="subtle" onClick={onSetUp}>Set up {name}</Button>}
    </Group>
  )
}

/**
 * The one line under a channel's form: what is still needed to save it, or
 * else the hint for filling it in.
 */
function FormNote({ missing, children }: { missing: string[]; children: ReactNode }) {
  return missing.length > 0
    ? <Text size={FS.meta} c={INK.pending}>Still needed: {missing.join(', ')}.</Text>
    : <Text size={FS.meta} c="dimmed">{children}</Text>
}

/** A write-only secret: empty keeps the configured one. */
function SecretInput({ label, description, set, value, onChange, required, first }: {
  label: string
  description?: string
  set: boolean
  value?: string
  onChange: (v: string | undefined) => void
  /** Shows the required mark, as the fields beside it do. */
  required?: boolean
  /** Marks the field "Set up" focuses. */
  first?: boolean
}) {
  return (
    <PasswordInput size="xs" label={label} description={description} autoComplete="new-password"
      withAsterisk={required} {...(first ? { 'data-first-field': true } : {})}
      placeholder={set ? 'Set. Type to replace it.' : 'Not set'}
      value={value ?? ''} onChange={(e) => onChange(e.currentTarget.value || undefined)} />
  )
}
