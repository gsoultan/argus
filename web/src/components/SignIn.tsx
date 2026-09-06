import { useState } from 'react'
import {
  Alert, Box, Button, Center, Divider, Paper, PasswordInput, Stack, Text,
  TextInput,
} from '@mantine/core'
import { IconAlertTriangle, IconKey, IconShieldLock } from '@tabler/icons-react'
import {
  loginURL, passwordLogin, verifyMFA, type Identity,
} from '~/lib/live'
import { FS } from '~/theme'

/**
 * The sign-in screen.
 *
 * Argus holds its own accounts, so this is a real form rather than a handoff.
 * An identity provider remains optional and appears alongside when one is
 * configured; a deployment with neither would have no way in at all, so the
 * screen says so rather than rendering an empty card.
 *
 * Two steps when a second factor is enrolled. The password screen never learns
 * whether an address exists — the control plane returns one message for every
 * refusal, and it is shown verbatim rather than reworded into something that
 * might imply more than the server said.
 */
export function SignIn({ identity, onSignedIn }: {
  identity: Identity
  onSignedIn: () => void
}) {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [code, setCode] = useState('')
  const [challenge, setChallenge] = useState<string>()
  const [useRecovery, setUseRecovery] = useState(false)
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  const submitPassword = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      const res = await passwordLogin(email, password)
      if (res.mfaRequired && res.challenge) {
        // The password is right, but it is not a sign-in on its own. Held only
        // in memory: a challenge in storage would outlive the tab it belongs to.
        setChallenge(res.challenge)
        setPassword('')
        return
      }
      onSignedIn()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Sign-in failed.')
    } finally {
      setBusy(false)
    }
  }

  const submitCode = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!challenge) return
    setBusy(true)
    setError(undefined)
    try {
      await verifyMFA(challenge, code, useRecovery)
      onSignedIn()
    } catch (err) {
      setError(err instanceof Error ? err.message : 'That code was not accepted.')
      setCode('')
    } finally {
      setBusy(false)
    }
  }

  return (
    <Center h="100vh">
      <Paper p="xl" withBorder maw={400} w="100%">
        <Brand />

        {identity.unreachable ? (
          <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />}>
            <Text size="xs">
              The control plane could not be reached. It may not be running, or the
              console may be pointed at the wrong address or scheme — it serves HTTPS
              when a certificate is configured.
            </Text>
          </Alert>
        ) : challenge ? (
          <form onSubmit={submitCode}>
            <Text size="sm" fw={600} mb={4}>Second factor</Text>
            <Text size="xs" c="dimmed" mb="md">
              {useRecovery
                ? 'Enter one of the recovery codes you saved when you enrolled. Each works once.'
                : 'Enter the six-digit code from your authenticator app.'}
            </Text>
            {error && <ErrorNote>{error}</ErrorNote>}
            <TextInput
              autoFocus
              label={useRecovery ? 'Recovery code' : 'Code'}
              placeholder={useRecovery ? 'xxxx-xxxx-xxxx-xxxx' : '123456'}
              value={code}
              onChange={(e) => setCode(e.currentTarget.value)}
              // A phone keypad for digits, and no autocorrect mangling either.
              inputMode={useRecovery ? 'text' : 'numeric'}
              autoComplete="one-time-code"
              spellCheck={false}
            />
            <Button type="submit" fullWidth mt="md" loading={busy} disabled={!code.trim()}>
              Verify
            </Button>
            <Button
              variant="subtle"
              color="slate"
              size="compact-xs"
              fullWidth
              mt="xs"
              onClick={() => {
                setUseRecovery((v) => !v)
                setCode('')
                setError(undefined)
              }}
            >
              {useRecovery ? 'Use my authenticator app' : 'I cannot use my authenticator app'}
            </Button>
          </form>
        ) : (
          <>
            {identity.passwordEnabled && (
              <form onSubmit={submitPassword}>
                {error && <ErrorNote>{error}</ErrorNote>}
                <Stack gap="sm">
                  <TextInput
                    autoFocus
                    label="Email"
                    type="email"
                    autoComplete="username"
                    value={email}
                    onChange={(e) => setEmail(e.currentTarget.value)}
                  />
                  <PasswordInput
                    label="Password"
                    autoComplete="current-password"
                    value={password}
                    onChange={(e) => setPassword(e.currentTarget.value)}
                  />
                </Stack>
                <Button
                  type="submit"
                  fullWidth
                  mt="md"
                  loading={busy}
                  disabled={!email.trim() || !password}
                  leftSection={<IconKey size={15} />}
                >
                  Sign in
                </Button>
              </form>
            )}

            {identity.passwordEnabled && identity.oidcEnabled && (
              <Divider my="lg" label="or" labelPosition="center" />
            )}

            {identity.oidcEnabled && (
              <Button
                fullWidth
                variant={identity.passwordEnabled ? 'default' : 'filled'}
                component="a"
                href={loginURL()}
                leftSection={<IconShieldLock size={15} />}
              >
                Sign in with SSO
              </Button>
            )}

            {!identity.passwordEnabled && !identity.oidcEnabled && (
              <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />}>
                <Text size="xs">
                  This control plane has no way to sign anyone in: no accounts and no
                  identity provider. Create the first account on the host with{' '}
                  <Text span ff="monospace" inherit>argus-control users add</Text>.
                </Text>
              </Alert>
            )}
          </>
        )}
      </Paper>
    </Center>
  )
}

function ErrorNote({ children }: { children: React.ReactNode }) {
  return (
    <Alert color="rose" variant="light" mb="md" icon={<IconAlertTriangle size={15} />}>
      <Text size="xs">{children}</Text>
    </Alert>
  )
}

function Brand() {
  return (
    <>
      <Box mb="md">
        <Box
          w={26}
          h={26}
          className="grid place-items-center shrink-0"
          style={{
            borderRadius: 7,
            background: 'linear-gradient(140deg, var(--color-verified), #0f766e)',
          }}
        >
          <IconShieldLock size={15} color="#04140f" stroke={2.4} />
        </Box>
      </Box>
      <Text fw={700} size="sm" style={{ letterSpacing: '0.02em' }}>ARGUS</Text>
      <Text size={FS.micro} c="dimmed" mb="lg" lh={1.5}>
        Every action here is attributed to a person, so there is no anonymous
        access — not even read-only.
      </Text>
    </>
  )
}
