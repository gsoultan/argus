import { useEffect, useState } from 'react'
import {
  Alert, Button, Center, Loader, Paper, PasswordInput, Stack, Text, TextInput,
} from '@mantine/core'
import { IconAlertTriangle, IconCircleCheck, IconKey, IconShieldLock } from '@tabler/icons-react'
import { completeSetup, setupStatus, type SetupStatus } from '~/lib/live'
import { FS } from '~/theme'
import { Brand, ErrorNote } from '~/components/SignIn'

/** The floor the control plane enforces; checked here only to say so sooner. */
const MIN_PASSWORD = 12

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
      <Paper p="xl" withBorder maw={440} w="100%" radius="lg">
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
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  const tooShort = password.length > 0 && [...password].length < MIN_PASSWORD
  const mismatch = repeat.length > 0 && repeat !== password
  const ready = token.trim() && email.trim() && [...password].length >= MIN_PASSWORD &&
    password === repeat

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
      <Alert color="sky" variant="light" icon={<IconShieldLock size={16} />} mb="md">
        <Text size="xs">
          The control plane wrote a one-time setup token on its host. Read it there:
        </Text>
        <Text size={FS.digest} ff="monospace" mt={6} c="slate.2">
          sudo cat {tokenFile ?? '/var/lib/argus/setup-token'}
        </Text>
      </Alert>
      {error && <ErrorNote>{error}</ErrorNote>}
      <Stack gap="sm">
        <TextInput
          autoFocus
          size="sm"
          label="Setup token"
          value={token}
          onChange={(e) => setToken(e.currentTarget.value)}
          autoComplete="off"
          spellCheck={false}
          styles={{ input: { fontFamily: 'var(--mantine-font-family-monospace)' } }}
        />
        <TextInput
          size="sm"
          label="Email"
          type="email"
          autoComplete="username"
          value={email}
          onChange={(e) => setEmail(e.currentTarget.value)}
        />
        <TextInput
          size="sm"
          label="Name"
          description="Optional. Shown next to everything this account does."
          autoComplete="name"
          value={name}
          onChange={(e) => setName(e.currentTarget.value)}
        />
        <PasswordInput
          size="sm"
          label="Password"
          description={`At least ${MIN_PASSWORD} characters. A passphrase is best.`}
          autoComplete="new-password"
          value={password}
          error={tooShort ? `${MIN_PASSWORD} characters at least` : undefined}
          onChange={(e) => setPassword(e.currentTarget.value)}
        />
        <PasswordInput
          size="sm"
          label="Repeat password"
          autoComplete="new-password"
          value={repeat}
          error={mismatch ? 'The two passwords do not match' : undefined}
          onChange={(e) => setRepeat(e.currentTarget.value)}
        />
      </Stack>
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
        Further accounts are added on the host with{' '}
        <Text span ff="monospace" inherit>argus-control users add</Text>.
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
      <Text size={FS.digest} ff="monospace" mt={6} c="slate.2">
        argus-control users add you@example.com --role admin
      </Text>
    </Alert>
  )
}
