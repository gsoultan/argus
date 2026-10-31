import { useEffect, useRef, useState } from 'react'
import {
  ActionIcon, Alert, Badge, Box, Button, CopyButton, Grid, Group, Loader, PasswordInput,
  SegmentedControl, Stack, Switch, Text, Textarea, TextInput, Tooltip,
} from '@mantine/core'
import {
  IconArrowLeft, IconArrowRight, IconCheck, IconCircleCheck, IconClipboard, IconCopy,
  IconDatabase, IconDice5, IconRocket, IconShieldLock, IconUpload,
} from '@tabler/icons-react'
import {
  checkInstallDatabase, install, waitForControlPlane, type DatabaseCheck, type DatabaseInput,
  type InstallResult, type SetupStatus,
} from '~/lib/live'
import { FS } from '~/theme'
import { ErrorNote } from '~/components/SignIn'
import { generatePassword } from '~/components/Setup'

const MIN_PASSWORD = 12

const STEPS = ['Setup token', 'Database', 'Addresses', 'Certificate', 'Administrator'] as const

/**
 * Installing Argus, in the browser.
 *
 * Shown by a control plane that has no configuration at all. Everything the
 * old install guide had people write into control.yaml and control.env by hand
 * is asked here instead, and the control plane writes the files itself --
 * secrets and certificates included. What it does not ask has a safe default,
 * and every setting is on the Settings page afterwards.
 *
 * Guarded by the same one-time token as the first-account form, for the same
 * reason: until this is done, the first person to find the address would
 * choose the database and the administrator.
 */
export function Install({ status }: { status: SetupStatus }) {
  const [step, setStep] = useState(0)
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)
  const [done, setDone] = useState<InstallResult>()

  const [token, setToken] = useState('')
  const [db, setDb] = useState<DatabaseInput>({
    host: 'localhost', port: 5432, name: 'argus', user: 'argus', password: '', sslMode: 'require',
  })
  const [dbCheck, setDbCheck] = useState<DatabaseCheck>()
  const port = status.port ?? 8080
  const [consoleUrl, setConsoleUrl] = useState(
    `https://${status.hostname ?? window.location.hostname}${port === 443 ? '' : `:${port}`}`,
  )
  const [fleetUrl, setFleetUrl] = useState('')
  const [listenPort, setListenPort] = useState(String(port))
  const [quic, setQuic] = useState(true)
  const [certMode, setCertMode] = useState<'own' | 'upload'>('own')
  const [certPem, setCertPem] = useState('')
  const [keyPem, setKeyPem] = useState('')
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [repeat, setRepeat] = useState('')
  const [shown, setShown] = useState(false)
  const [generated, setGenerated] = useState(false)

  const keepsAccounts = dbCheck?.accountsExist === true
  const lastStep = keepsAccounts ? 3 : 4

  const portNumber = Number(listenPort)
  const ready = [
    token.trim() !== '',
    dbCheck !== undefined,
    consoleUrl.startsWith('https://') && Number.isInteger(portNumber) && portNumber > 0 && portNumber < 65536,
    certMode === 'own' || (certPem.trim() !== '' && keyPem.trim() !== ''),
    keepsAccounts ||
      (email.includes('@') && [...password].length >= MIN_PASSWORD && password === repeat),
  ]

  const test = async () => {
    setBusy(true)
    setError(undefined)
    setDbCheck(undefined)
    try {
      setDbCheck(await checkInstallDatabase(token.trim(), db))
    } catch (err) {
      setError(err instanceof Error ? err.message : 'The database could not be reached.')
    } finally {
      setBusy(false)
    }
  }

  const finish = async () => {
    setBusy(true)
    setError(undefined)
    try {
      const res = await install({
        token: token.trim(), database: db, consoleUrl, fleetUrl,
        port: portNumber, quic,
        certPem: certMode === 'upload' ? certPem : '',
        keyPem: certMode === 'upload' ? keyPem : '',
        admin: { email, displayName: name, password },
      })
      setDone(res)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Setup failed.')
    } finally {
      setBusy(false)
    }
  }

  if (done) return <Installed result={done} />

  const next = () => {
    setError(undefined)
    setStep((s) => Math.min(s + 1, lastStep))
  }

  return (
    <Box>
      <Text size="sm" fw={600} mb={4}>Set up Argus</Text>
      <Text size="xs" c="dimmed" mb="md">
        This control plane has no configuration yet. Answer these and it writes its own,
        with its secrets and certificates, then starts. Everything here can be changed
        later under Settings.
      </Text>
      <Group gap={6} mb="md" wrap="wrap">
        {STEPS.slice(0, lastStep + 1).map((label, i) => (
          <Badge
            key={label}
            size="sm"
            variant={i === step ? 'filled' : 'light'}
            color={i < step ? 'teal' : i === step ? 'azure' : 'slate'}
            leftSection={i < step ? <IconCheck size={11} /> : undefined}
            style={{ cursor: i < step ? 'pointer' : undefined }}
            onClick={() => i < step && setStep(i)}
          >
            {i + 1}. {label}
          </Badge>
        ))}
      </Group>
      {error && <ErrorNote>{error}</ErrorNote>}

      {step === 0 && (
        <TokenStep tokenFile={status.tokenFile} token={token} setToken={setToken} onError={setError} />
      )}

      {step === 1 && (
        <Stack gap="sm">
          <Text size="xs" c="dimmed">
            The PostgreSQL database Argus keeps its records in, version 13 or newer. Create
            an empty database and a user that owns it; Argus makes its tables itself.
          </Text>
          <Grid gap="sm">
            <Grid.Col span={{ base: 12, sm: 8 }}>
              <TextInput size="sm" label="Server" value={db.host}
                onChange={(e) => { setDb({ ...db, host: e.currentTarget.value }); setDbCheck(undefined) }} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 4 }}>
              <TextInput size="sm" label="Port" inputMode="numeric" value={String(db.port || '')}
                onChange={(e) => { setDb({ ...db, port: Number(e.currentTarget.value) || 0 }); setDbCheck(undefined) }} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 4 }}>
              <TextInput size="sm" label="Database" value={db.name}
                onChange={(e) => { setDb({ ...db, name: e.currentTarget.value }); setDbCheck(undefined) }} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 4 }}>
              <TextInput size="sm" label="User" value={db.user} autoComplete="off"
                onChange={(e) => { setDb({ ...db, user: e.currentTarget.value }); setDbCheck(undefined) }} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 4 }}>
              <PasswordInput size="sm" label="Password" value={db.password} autoComplete="new-password"
                onChange={(e) => { setDb({ ...db, password: e.currentTarget.value }); setDbCheck(undefined) }} />
            </Grid.Col>
          </Grid>
          <Box>
            <Text size="sm" fw={500} mb={4}>Encryption</Text>
            <SegmentedControl
              size="xs"
              value={db.sslMode}
              onChange={(v) => { setDb({ ...db, sslMode: v }); setDbCheck(undefined) }}
              data={[
                { value: 'require', label: 'Require TLS' },
                { value: 'verify-full', label: 'Require and verify' },
                { value: 'disable', label: 'None (same host only)' },
              ]}
            />
          </Box>
          <Group gap="sm">
            <Button size="xs" variant="light" leftSection={<IconDatabase size={14} />}
              loading={busy} onClick={() => void test()}>
              Test connection
            </Button>
            {dbCheck && (
              <Text size="xs" c="teal.4">
                <IconCircleCheck size={13} style={{ verticalAlign: -2 }} /> Connected to PostgreSQL{' '}
                {dbCheck.version.split(' ')[0]}.
              </Text>
            )}
          </Group>
          {keepsAccounts && (
            <Alert color="sky" variant="light" p="xs">
              <Text size="xs">
                This database already has Argus accounts, so they are kept and no new
                administrator is made. Sign in with one of them when setup finishes.
              </Text>
            </Alert>
          )}
        </Stack>
      )}

      {step === 2 && (
        <Stack gap="sm">
          <TextInput
            size="sm"
            label="Console address"
            description="Where people open Argus. Alerts link to it, and the certificate covers its name."
            value={consoleUrl}
            error={consoleUrl && !consoleUrl.startsWith('https://') ? 'Starts with https://' : undefined}
            onChange={(e) => setConsoleUrl(e.currentTarget.value)}
          />
          <TextInput
            size="sm"
            label="Address servers use"
            description="Optional. Only when servers reach this control plane at a different address from people, such as behind a Cloudflare Tunnel."
            placeholder="https://argus-internal.example.com:8080"
            value={fleetUrl}
            onChange={(e) => setFleetUrl(e.currentTarget.value)}
          />
          <Grid gap="sm" align="flex-end">
            <Grid.Col span={{ base: 12, sm: 4 }}>
              <TextInput size="sm" label="Port" inputMode="numeric" value={listenPort}
                onChange={(e) => setListenPort(e.currentTarget.value)} />
            </Grid.Col>
            <Grid.Col span={{ base: 12, sm: 8 }}>
              <Switch
                size="sm"
                checked={quic}
                onChange={(e) => setQuic(e.currentTarget.checked)}
                label="Let agents use QUIC on the same UDP port"
                description="Smoother sessions on a lossy network. Open the port for UDP too."
              />
            </Grid.Col>
          </Grid>
        </Stack>
      )}

      {step === 3 && (
        <Stack gap="sm">
          <SegmentedControl
            size="xs"
            value={certMode}
            onChange={(v) => setCertMode(v as 'own' | 'upload')}
            data={[
              { value: 'own', label: 'Make one for me' },
              { value: 'upload', label: 'Use my certificate' },
            ]}
          />
          {certMode === 'own' ? (
            <Alert color="sky" variant="light" icon={<IconShieldLock size={16} />}>
              <Text size="xs">
                The control plane keeps a small certificate authority of its own, issues its
                certificate from it, and renews it before it expires.
                Browsers warn about it until you trust that authority; Settings has it to
                download. Upload a certificate from your own CA there at any time.
              </Text>
            </Alert>
          ) : (
            <>
              <PemField label="Certificate (PEM, chain included)" value={certPem} onChange={setCertPem} />
              <PemField label="Private key (PEM)" value={keyPem} onChange={setKeyPem} secret />
            </>
          )}
        </Stack>
      )}

      {step === 4 && (
        <Grid gap="sm">
          <Grid.Col span={{ base: 12, sm: 6 }}>
            <TextInput size="sm" label="Email" description="What you sign in with." type="email"
              autoComplete="username" value={email} onChange={(e) => setEmail(e.currentTarget.value)} />
          </Grid.Col>
          <Grid.Col span={{ base: 12, sm: 6 }}>
            <TextInput size="sm" label="Name" description="Optional." autoComplete="name"
              value={name} onChange={(e) => setName(e.currentTarget.value)} />
          </Grid.Col>
          <Grid.Col span={{ base: 12, sm: 6 }}>
            <PasswordInput size="sm" label="Password" description={`At least ${MIN_PASSWORD} characters.`}
              autoComplete="new-password" value={password} visible={shown} onVisibilityChange={setShown}
              onChange={(e) => { setPassword(e.currentTarget.value); setGenerated(false) }} />
          </Grid.Col>
          <Grid.Col span={{ base: 12, sm: 6 }}>
            <PasswordInput size="sm" label="Repeat password" description={generated ? 'Filled in for you.' : ' '}
              autoComplete="new-password" value={repeat} visible={shown} onVisibilityChange={setShown}
              error={repeat && repeat !== password ? 'The two passwords do not match' : undefined}
              onChange={(e) => setRepeat(e.currentTarget.value)} />
          </Grid.Col>
          <Grid.Col span={12}>
            <Group gap="xs">
              <Button variant="light" color="slate" size="xs" leftSection={<IconDice5 size={14} />}
                onClick={() => {
                  const pw = generatePassword()
                  setPassword(pw); setRepeat(pw); setShown(true); setGenerated(true)
                }}>
                Generate password
              </Button>
              {generated && (
                <CopyButton value={password} timeout={1200}>
                  {({ copied, copy }) => (
                    <Button variant="subtle" color={copied ? 'teal' : 'slate'} size="xs"
                      leftSection={copied ? <IconCheck size={14} /> : <IconCopy size={14} />} onClick={copy}>
                      {copied ? 'Copied' : 'Copy password'}
                    </Button>
                  )}
                </CopyButton>
              )}
            </Group>
            {generated && (
              <Text size="xs" c="amber.4" mt={6}>
                Save this password in your password manager before you continue. It is not shown again.
              </Text>
            )}
          </Grid.Col>
        </Grid>
      )}

      <Group justify="space-between" mt="lg">
        <Button variant="subtle" color="slate" size="sm" leftSection={<IconArrowLeft size={14} />}
          disabled={step === 0 || busy} onClick={() => { setError(undefined); setStep(step - 1) }}>
          Back
        </Button>
        {step < lastStep ? (
          <Button size="sm" rightSection={<IconArrowRight size={14} />} disabled={!ready[step]} onClick={next}>
            Next
          </Button>
        ) : (
          <Button size="sm" leftSection={<IconRocket size={15} />} loading={busy}
            disabled={!ready.slice(0, lastStep + 1).every(Boolean)} onClick={() => void finish()}>
            Finish setup
          </Button>
        )}
      </Group>
    </Box>
  )
}

/** The one-time token, and how to read it on the host. */
export function TokenStep({ tokenFile, token, setToken, onError }: {
  tokenFile?: string
  token: string
  setToken: (t: string) => void
  onError: (msg: string) => void
}) {
  const command = `sudo cat ${tokenFile ?? '/var/lib/argus-control/setup-token'}`
  // Paste, never generate. The token proves the person filling this in can
  // read a file on the host; one made in this browser would prove nothing.
  const paste = async () => {
    try {
      setToken((await navigator.clipboard.readText()).trim())
    } catch {
      onError('The browser would not read the clipboard. Paste the token into the field instead.')
    }
  }
  return (
    <Stack gap="sm">
      <Text size="xs" c="dimmed">
        The control plane wrote a one-time token to a file only its own account can read.
        It proves you have the host, so nobody who merely found this page can set it up.
      </Text>
      <Alert color="sky" variant="light" icon={<IconShieldLock size={16} />} p="xs">
        <Text size="xs">Read it on the host:</Text>
        <Group gap={6} mt={4} wrap="nowrap">
          <Text size={FS.digest} ff="monospace" c="slate.7" style={{ wordBreak: 'break-all' }}>
            {command}
          </Text>
          <CopyButton value={command} timeout={1200}>
            {({ copied, copy }) => (
              <Tooltip label={copied ? 'Copied' : 'Copy command'}>
                <ActionIcon variant="subtle" color={copied ? 'teal' : 'slate'} size="sm"
                  onClick={copy} aria-label="Copy command">
                  {copied ? <IconCheck size={13} /> : <IconCopy size={13} />}
                </ActionIcon>
              </Tooltip>
            )}
          </CopyButton>
        </Group>
      </Alert>
      <TextInput
        autoFocus
        size="sm"
        label="Setup token"
        value={token}
        onChange={(e) => setToken(e.currentTarget.value)}
        autoComplete="off"
        spellCheck={false}
        styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
        rightSection={
          <Tooltip label="Paste from clipboard">
            <ActionIcon variant="subtle" color="slate" onClick={() => void paste()} aria-label="Paste token">
              <IconClipboard size={15} />
            </ActionIcon>
          </Tooltip>
        }
      />
    </Stack>
  )
}

/** A PEM pasted or loaded from a file. */
export function PemField({ label, value, onChange, secret }: {
  label: string
  value: string
  onChange: (v: string) => void
  secret?: boolean
}) {
  const file = useRef<HTMLInputElement>(null)
  return (
    <Box>
      <Textarea
        size="xs"
        label={label}
        placeholder={secret ? '-----BEGIN PRIVATE KEY-----' : '-----BEGIN CERTIFICATE-----'}
        autosize
        minRows={3}
        maxRows={8}
        value={value}
        spellCheck={false}
        onChange={(e) => onChange(e.currentTarget.value)}
        styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)', fontSize: 11 } }}
      />
      <input
        ref={file}
        type="file"
        hidden
        onChange={(e) => {
          const f = e.currentTarget.files?.[0]
          if (f) void f.text().then(onChange)
          e.currentTarget.value = ''
        }}
      />
      <Button size="compact-xs" variant="subtle" color="slate" mt={4}
        leftSection={<IconUpload size={12} />} onClick={() => file.current?.click()}>
        Load from a file
      </Button>
    </Box>
  )
}

/** Written; the control plane is restarting on its new configuration. */
function Installed({ result }: { result: InstallResult }) {
  const [up, setUp] = useState<boolean>()
  useEffect(() => {
    void waitForControlPlane().then(setUp)
  }, [])
  // The console's address may not be this one: go where the configuration
  // says people sign in.
  const target = (() => {
    try {
      return new URL(result.consoleUrl).origin === window.location.origin ? '/' : result.consoleUrl
    } catch {
      return '/'
    }
  })()
  return (
    <>
      <Alert color="teal" variant="light" icon={<IconCircleCheck size={16} />} mb="md">
        <Text size="xs">
          Argus is set up. Its configuration is in{' '}
          <Text span ff="monospace" inherit>{result.configFile}</Text>
          {result.email ? `, and ${result.email} is an administrator.` : '.'} Sign in, then enrol
          a second factor straight away.
        </Text>
      </Alert>
      {up === undefined ? (
        <Group gap="xs" mb="md">
          <Loader size="xs" color="azure" />
          <Text size="xs" c="dimmed">Starting the control plane…</Text>
        </Group>
      ) : up === false ? (
        <Alert color="amber" variant="light" mb="md">
          <Text size="xs">
            The control plane has not answered yet. Its log says why:{' '}
            <Text span ff="monospace" inherit>journalctl -u argus-control -n 30</Text>
          </Text>
        </Alert>
      ) : null}
      <Button size="sm" fullWidth disabled={up === undefined} onClick={() => window.location.assign(target)}>
        Go to sign in
      </Button>
    </>
  )
}
