import { useMemo, useState } from 'react'
import {
  Alert, Anchor, Box, Button, Center, Code, CopyButton, Group, Modal, Paper, PasswordInput,
  SimpleGrid, Stack, Text, TextInput,
} from '@mantine/core'
import { IconAlertTriangle, IconCopy, IconDownload, IconShieldLock } from '@tabler/icons-react'
import qrcode from 'qrcode-generator'
import { beginMFAEnrolment, confirmMFAEnrolment, logout } from '~/lib/live'
import { FS } from '~/theme'
import { Brand, ErrorNote } from '~/components/SignIn'

/**
 * Setting up, or replacing, the second step of signing in.
 *
 * Four steps, each one a screen: prove it is you (password, and the current
 * code when replacing one), scan the new secret, prove the app holds it, then
 * save the recovery codes. Until the third step succeeds the old second step
 * stays in force -- the control plane keeps the new secret apart until then --
 * so walking away half-way changes nothing.
 *
 * The secret and the recovery codes live only in this component's state and
 * are never written to storage: closing it forgets them.
 */
export function TwoFactorSetup({ enrolled, onDone }: {
  /** Whether a second step already exists, so this replaces it. */
  enrolled: boolean
  onDone: () => void
}) {
  const [stage, setStage] = useState<'prove' | 'scan' | 'codes'>('prove')
  const [password, setPassword] = useState('')
  const [current, setCurrent] = useState('')
  const [useRecovery, setUseRecovery] = useState(false)
  const [secret, setSecret] = useState<{ secret: string; uri: string }>()
  const [code, setCode] = useState('')
  const [codes, setCodes] = useState<string[]>([])
  const [error, setError] = useState<string>()
  const [busy, setBusy] = useState(false)

  const begin = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      setSecret(await beginMFAEnrolment(password, current.trim(), useRecovery))
      setPassword('')
      setCurrent('')
      setStage('scan')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Could not start.')
    } finally {
      setBusy(false)
    }
  }

  const confirm = async (e: React.FormEvent) => {
    e.preventDefault()
    setBusy(true)
    setError(undefined)
    try {
      const out = await confirmMFAEnrolment(code.trim())
      setCodes(out.recoveryCodes)
      setSecret(undefined)
      setStage('codes')
    } catch (err) {
      setError(err instanceof Error ? err.message : 'That code was not accepted.')
      setCode('')
    } finally {
      setBusy(false)
    }
  }

  if (stage === 'prove') {
    return (
      <form onSubmit={begin}>
        <Text size="sm" mb="md">
          {enrolled
            ? 'Replacing your authenticator. Your current one keeps working until the new one is confirmed.'
            : 'Each sign-in will ask for a six-digit code from an authenticator app on your phone, as well as your password.'}
        </Text>
        {error && <ErrorNote>{error}</ErrorNote>}
        <Stack gap="sm">
          <PasswordInput
            autoFocus
            size="sm"
            label="Your password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => setPassword(e.currentTarget.value)}
          />
          {enrolled && (
            <TextInput
              size="sm"
              label={useRecovery ? 'A recovery code' : 'Code from your current authenticator'}
              placeholder={useRecovery ? 'xxxx-xxxx-xxxx-xxxx' : '123456'}
              inputMode={useRecovery ? 'text' : 'numeric'}
              autoComplete="one-time-code"
              spellCheck={false}
              value={current}
              onChange={(e) => setCurrent(e.currentTarget.value)}
              description={
                <Anchor component="button" type="button" size="xs" onClick={() => {
                  setUseRecovery((v) => !v)
                  setCurrent('')
                }}>
                  {useRecovery ? 'Use my authenticator instead' : 'I no longer have my authenticator'}
                </Anchor>
              }
            />
          )}
        </Stack>
        <Button type="submit" size="sm" fullWidth mt="md" loading={busy}
          disabled={!password || (enrolled && !current.trim())}>
          Continue
        </Button>
      </form>
    )
  }

  if (stage === 'scan' && secret) {
    return (
      <form onSubmit={confirm}>
        <Text size="sm" mb="sm">
          Scan this with an authenticator app (Google Authenticator, 1Password, Authy, Microsoft
          Authenticator and others), then enter the code it shows.
        </Text>
        <Center mb="sm">
          <QR text={secret.uri} />
        </Center>
        <Text size={FS.meta} c="dimmed" ta="center">
          Cannot scan? Enter this key by hand, or{' '}
          <Anchor href={secret.uri} size={FS.meta}>open it on this device</Anchor>.
        </Text>
        <Group justify="center" gap={6} mt={4} mb="md">
          <Code fz={FS.meta} style={{ letterSpacing: '0.08em' }}>{groupKey(secret.secret)}</Code>
          <CopyButton value={secret.secret}>
            {({ copied, copy }) => (
              <Button size="compact-xs" variant="subtle" onClick={copy} leftSection={<IconCopy size={12} />}>
                {copied ? 'Copied' : 'Copy'}
              </Button>
            )}
          </CopyButton>
        </Group>
        {error && <ErrorNote>{error}</ErrorNote>}
        <TextInput
          autoFocus
          size="sm"
          label="Code from the app"
          placeholder="123456"
          inputMode="numeric"
          autoComplete="one-time-code"
          spellCheck={false}
          value={code}
          onChange={(e) => setCode(e.currentTarget.value)}
        />
        <Button type="submit" size="sm" fullWidth mt="md" loading={busy} disabled={code.trim().length !== 6}>
          Turn it on
        </Button>
      </form>
    )
  }

  return <RecoveryCodes codes={codes} onDone={onDone} />
}

/** The recovery codes, shown once, with a way to keep them. */
function RecoveryCodes({ codes, onDone }: { codes: string[]; onDone: () => void }) {
  const [saved, setSaved] = useState(false)
  const text = codes.join('\n')
  const download = () => {
    const blob = new Blob([
      `Argus recovery codes for ${window.location.host}\n` +
      'Each one signs in once, in place of your authenticator.\n\n' + text + '\n',
    ], { type: 'text/plain' })
    const a = document.createElement('a')
    a.href = URL.createObjectURL(blob)
    a.download = 'argus-recovery-codes.txt'
    a.click()
    URL.revokeObjectURL(a.href)
    setSaved(true)
  }
  return (
    <Stack gap="sm">
      <Alert color="teal" variant="light" icon={<IconShieldLock size={16} />}>
        <Text size="xs">
          Two-step sign-in is on. Every browser signed in to this account was signed out, except this one.
        </Text>
      </Alert>
      <Text size="sm">
        Save these recovery codes somewhere safe, such as your password manager. If you lose
        your phone, each one signs you in once. They are not shown again.
      </Text>
      <SimpleGrid cols={2} spacing={4}>
        {codes.map((c) => <Code key={c} fz={FS.meta} ta="center">{c}</Code>)}
      </SimpleGrid>
      <Group grow>
        <CopyButton value={text}>
          {({ copied, copy }) => (
            <Button size="xs" variant="light" leftSection={<IconCopy size={14} />}
              onClick={() => { copy(); setSaved(true) }}>
              {copied ? 'Copied' : 'Copy'}
            </Button>
          )}
        </CopyButton>
        <Button size="xs" variant="light" leftSection={<IconDownload size={14} />} onClick={download}>
          Download
        </Button>
      </Group>
      <Button size="sm" onClick={onDone} disabled={!saved}>
        {saved ? 'Done' : 'Copy or download them first'}
      </Button>
    </Stack>
  )
}

/** A QR code drawn as SVG, so nothing about the secret leaves the page. */
function QR({ text }: { text: string }) {
  const svg = useMemo(() => {
    const q = qrcode(0, 'M')
    q.addData(text)
    q.make()
    return q.createSvgTag({ cellSize: 4, margin: 2, scalable: true })
  }, [text])
  return (
    <Box
      w={176}
      h={176}
      bg="white"
      p={4}
      style={{ borderRadius: 8 }}
      aria-label="QR code for your authenticator app"
      role="img"
      // Generated here from the enrolment URI, never from anything a user typed.
      dangerouslySetInnerHTML={{ __html: svg }}
    />
  )
}

function groupKey(secret: string): string {
  return secret.match(/.{1,4}/g)?.join(' ') ?? secret
}

/** The console's own modal for setting up or replacing it from the account menu. */
export function TwoFactorModal({ opened, enrolled, onClose }: {
  opened: boolean
  enrolled: boolean
  onClose: () => void
}) {
  return (
    <Modal opened={opened} onClose={onClose} title="Two-step sign-in" size="sm" centered>
      {opened && <TwoFactorSetup enrolled={enrolled} onDone={onClose} />}
    </Modal>
  )
}

/**
 * The whole screen, for an account whose role must have a second step and has
 * none. The control plane refuses everything else until it is set up, so there
 * is nothing else to show.
 */
export function TwoFactorRequired({ email, onDone }: { email?: string; onDone: () => void }) {
  return (
    <Center mih="100vh" p="md">
      <Paper p="xl" withBorder maw={440} w="100%" radius="lg">
        <Brand />
        <Alert color="amber" variant="light" icon={<IconAlertTriangle size={16} />} mb="md">
          <Text size="xs">
            Your role can change who reaches which servers, so it needs two-step sign-in before
            you can continue{email ? ` as ${email}` : ''}.
          </Text>
        </Alert>
        <TwoFactorSetup enrolled={false} onDone={onDone} />
        <Button variant="subtle" color="slate" size="compact-xs" fullWidth mt="md"
          onClick={async () => {
            await logout()
            window.location.reload()
          }}>
          Sign out
        </Button>
      </Paper>
    </Center>
  )
}
