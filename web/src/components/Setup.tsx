import { useEffect, useState } from 'react'
import {
  ActionIcon, Alert, Button, Center, CopyButton, Group, Loader, Paper, PasswordInput,
  Grid, Text, TextInput, Tooltip,
} from '@mantine/core'
import {
  IconAlertTriangle, IconCheck, IconCircleCheck, IconClipboard, IconCopy, IconDice5, IconKey,
  IconShieldLock,
} from '@tabler/icons-react'
import { completeSetup, setupStatus, type SetupStatus } from '~/lib/live'
import { FS } from '~/theme'
import { Brand, ErrorNote } from '~/components/SignIn'
import { Install } from '~/components/Install'

/** The floor the control plane enforces; checked here only to say so sooner. */
const MIN_PASSWORD = 12

/** No 0/O, 1/l/I: a generated password gets read off a screen and typed. */
const PASSWORD_ALPHABET = 'abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789'

/**
 * Four groups of six from a 57-letter alphabet: about 140 bits, far past what
 * the argon2 hash behind it needs, and grouped so it can be checked by eye.
 * Rejection sampling, so no letter is likelier than another.
 */
export function generatePassword(): string {
  const out: string[] = []
  const limit = 256 - (256 % PASSWORD_ALPHABET.length)
  const buf = new Uint8Array(64)
  while (out.length < 24) {
    crypto.getRandomValues(buf)
    for (const b of buf) {
      if (b < limit && out.length < 24) out.push(PASSWORD_ALPHABET[b % PASSWORD_ALPHABET.length]!)
    }
  }
  return [0, 6, 12, 18].map((i) => out.slice(i, i + 6).join('')).join('-')
}

/**
 * First-run setup: the first account, made in the browser.
 *
 * The form asks for a token the control plane wrote to a file on its host,
 * because without one this page would belong to whoever found it first -- and
 * behind a Cloudflare Tunnel that is anyone on the internet. Reading the file
 * takes the same shell on the host that `argus-control users add` does, so the
 * page is a convenience over that command, never a way around it.
 *
 * Every state the server can be in gets its own screen, so a closed setup does
 * not look like a form that is about to work.
 */
export function Setup() {
  const [status, setStatus] = useState<SetupStatus>()
  const [loadError, setLoadError] = useState<string>()
  const [created, setCreated] = useState<string>()

  useEffect(() => {
    setupStatus().then(setStatus, (err: unknown) =>
      setLoadError(err instanceof Error ? err.message : 'The control plane did not answer.'))
  }, [])

  return (
    <Center mih="100vh" py="xl">
      <Paper p="xl" withBorder maw={status && status.available && !created ? 720 : 440} w="100%" radius="lg" mx="md">
        <Brand />
        {created ? (
          <Done email={created} />
        ) : loadError ? (
          <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />}>
            <Text size="xs">Could not ask the control plane whether setup is open: {loadError}</Text>
          </Alert>
        ) : !status ? (
          <Center py="lg"><Loader size="sm" color="azure" /></Center>
        ) : status.complete ? (
          <Closed />
        ) : !status.available ? (
          <NotOffered />
        ) : status.mode === 'install' ? (
          <Install status={status} />
        ) : (
          <SetupForm tokenFile={status.tokenFile} onCreated={setCreated} />
        )}
      </Paper>
    </Center>
  )
}

function SetupForm({ tokenFile, onCreated }: {
  tokenFile?: string
  onCreated: (email: string) => void
}) {
  const [token, setToken] = useState('')
  const [email, setEmail] = useState('')
  const [name, setName] = useState('')
  const [password, setPassword] = useState('')
  const [repeat, setRepeat] = useState('')
  const [shown, setShown] = useState(false)
  const [generated, setGenerated] = useState(false)
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  const tooShort = password.length > 0 && [...password].length < MIN_PASSWORD
  const mismatch = repeat.length > 0 && repeat !== password
  const ready = token.trim() && email.trim() && [...password].length >= MIN_PASSWORD &&
    password === repeat
  const command = `sudo cat ${tokenFile ?? '/var/lib/argus-control/setup-token'}`

  // Shown, not hidden: a password nobody saw is one nobody saved. Filling the
  // repeat field too is the point -- retyping 27 random characters to prove
  // you can is not a check worth asking for.
  const generate = () => {
    const pw = generatePassword()
    setPassword(pw)
    setRepeat(pw)
    setShown(true)
    setGenerated(true)
  }

  // Paste, never generate. The token proves the person filling this in can
  // read a file on the host; one made in this browser would prove nothing.
  const paste = async () => {
    try {
      setToken((await navigator.clipboard.readText()).trim())
    } catch {
      setError('The browser would not read the clipboard. Paste the token into the field instead.')
    }
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      const res = await completeSetup({ token: token.trim(), email, displayName: name, password })
      onCreated(res.email)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Setup failed.')
    } finally {
      setBusy(false)
    }
  }

  return (
    <form onSubmit={submit}>
      <Text size="sm" fw={600} mb={4}>Set up Argus</Text>
      <Text size="xs" c="dimmed" mb="md">
        Create the first administrator. This page works once: as soon as an account
        exists it closes for good.
      </Text>
      {error && <ErrorNote>{error}</ErrorNote>}
      {/* Grid rather than SimpleGrid: its stylesheet is not among the ones
          app.css imports, so SimpleGrid would render one column everywhere. */}
      <Grid gap="sm">
        <Grid.Col span={{ base: 12, sm: 6 }}>
          <TextInput
            autoFocus
            size="sm"
            label="Setup token"
            description="Written on the control plane's host. It works once."
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
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 6 }}>
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
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 6 }}>
          <TextInput
            size="sm"
            label="Email"
            description="What you sign in with."
            type="email"
            autoComplete="username"
            value={email}
            onChange={(e) => setEmail(e.currentTarget.value)}
          />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 6 }}>
          <TextInput
            size="sm"
            label="Name"
            description="Optional. Shown next to everything this account does."
            autoComplete="name"
            value={name}
            onChange={(e) => setName(e.currentTarget.value)}
          />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 6 }}>
          <PasswordInput
            size="sm"
            label="Password"
            description={`At least ${MIN_PASSWORD} characters, or generate one.`}
            autoComplete="new-password"
            value={password}
            visible={shown}
            onVisibilityChange={setShown}
            error={tooShort ? `${MIN_PASSWORD} characters at least` : undefined}
            onChange={(e) => {
              setPassword(e.currentTarget.value)
              setGenerated(false)
            }}
            styles={generated ? { input: { fontFamily: 'var(--mantine-font-family-monospace)' } } : undefined}
          />
        </Grid.Col>
        <Grid.Col span={{ base: 12, sm: 6 }}>
          <PasswordInput
            size="sm"
            label="Repeat password"
            description={generated ? 'Filled in for you.' : '\u00a0'}
            autoComplete="new-password"
            value={repeat}
            visible={shown}
            onVisibilityChange={setShown}
            error={mismatch ? 'The two passwords do not match' : undefined}
            onChange={(e) => setRepeat(e.currentTarget.value)}
            styles={generated ? { input: { fontFamily: 'var(--mantine-font-family-monospace)' } } : undefined}
          />
        </Grid.Col>
      </Grid>
      <Group gap="xs" mt="xs">
        <Button variant="light" color="slate" size="xs" leftSection={<IconDice5 size={14} />}
          onClick={generate}>
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
      <Button
        type="submit"
        size="sm"
        fullWidth
        mt="md"
        loading={busy}
        disabled={!ready}
        leftSection={<IconKey size={15} />}
      >
        Create administrator
      </Button>
    </form>
  )
}

function Done({ email }: { email: string }) {
  return (
    <>
      <Alert color="teal" variant="light" icon={<IconCircleCheck size={16} />} mb="md">
        <Text size="xs">
          {email} is an administrator, and setup is now closed. Sign in, then enrol a
          second factor straight away: a privileged account with only a password is one
          phish away from every host Argus brokers.
        </Text>
      </Alert>
      <Button size="sm" fullWidth onClick={() => window.location.assign('/')}>
        Sign in
      </Button>
    </>
  )
}

function Closed() {
  return (
    <>
      <Text size="sm" fw={600} mb={4}>Setup is complete</Text>
      <Text size="xs" c="dimmed" mb="md">
        This control plane already has an account, so there is nothing to set up.
        An administrator adds further accounts under Settings.
      </Text>
      <Button size="sm" fullWidth onClick={() => window.location.assign('/')}>
        Go to sign in
      </Button>
    </>
  )
}

function NotOffered() {
  return (
    <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />}>
      <Text size="xs">
        This control plane is not offering setup in the browser: it was started with{' '}
        <Text span ff="monospace" inherit>web_setup: false</Text>, has no signing secret, or
        could not write its setup token (its log says which). Create the first account on
        the host instead:
      </Text>
      <Text size={FS.digest} ff="monospace" mt={6} c="slate.7">
        argus-control users add you@example.com --role admin
      </Text>
    </Alert>
  )
}
